package rclone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/utils/keymutex"
)

// StateDir holds a directory per rclone mount with its mount point, config, rc socket and VFS cache.
var StateDir = "/var/lib/csi-rclone"

// volumeDir is named by a hash, because volume IDs may contain any character
// and the rc socket path has to stay short.
func volumeDir(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return filepath.Join(StateDir, hex.EncodeToString(sum[:8]))
}

func rcSocket(volumeID string) string {
	return filepath.Join(volumeDir(volumeID), "rc.sock")
}

// volumeLocks serializes all node calls per rclone mount. Kubelet retries a slow
// stage while the first one is still mounting.
var volumeLocks = keymutex.NewHashed(0)

func lockVolume(volumeID string)   { volumeLocks.LockKey(volumeID) }
func unlockVolume(volumeID string) { _ = volumeLocks.UnlockKey(volumeID) }

// volumeState is everything a later plugin instance needs to mount an rclone
// mount again. Kubelet only passes secrets and volume context to NodeStageVolume.
type volumeState struct {
	// VolumeID names the mount, see mountID
	VolumeID string `json:"volumeID"`
	// StagingPath is where rclone is mounted
	StagingPath string   `json:"stagingPath"`
	Args        []string `json:"args"`
	Env         []string `json:"env"`
	// Users are the kubelet staging paths of persistent volumes and the targets
	// of ephemeral volumes on this mount. The last one to leave unmounts it.
	Users map[string]struct{} `json:"users,omitempty"`
	// Targets maps each publish target to whether it is bound read-only
	Targets map[string]bool `json:"targets,omitempty"`
	// Mounted tells an rclone that exited cleanly apart from a mount that never came up
	Mounted bool `json:"mounted,omitempty"`
}

func statePath(volumeID string) string {
	return filepath.Join(volumeDir(volumeID), "state.json")
}

// saveState writes atomically, so neither a crash nor a reconcile pass sees a half-written file.
func saveState(st volumeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	final := statePath(st.VolumeID)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func loadState(volumeID string) (volumeState, error) {
	return readState(statePath(volumeID))
}

func readState(file string) (volumeState, error) {
	var st volumeState
	data, err := os.ReadFile(file)
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	// 4.0.0 mounted rclone at the kubelet staging path, its only user
	if err == nil && st.Users == nil && st.StagingPath != mountPath(st.VolumeID) {
		st.Users = map[string]struct{}{st.StagingPath: {}}
	}
	return st, err
}

// mountPath is where rclone is mounted, and bound from into staging and target paths.
func mountPath(volumeID string) string {
	return filepath.Join(volumeDir(volumeID), "mnt")
}

func (st volumeState) uses(path string) bool {
	_, user := st.Users[path]
	_, target := st.Targets[path]
	return user || target
}

// lockMountOf finds the mount that path uses or is bound from, and returns it locked.
func lockMountOf(path string) (volumeState, bool) {
	for _, found := range loadStates() {
		if !found.uses(path) {
			continue
		}
		lockVolume(found.VolumeID)
		// Reloaded under the lock, path may have left meanwhile
		if st, err := loadState(found.VolumeID); err == nil && st.uses(path) {
			return st, true
		}
		unlockVolume(found.VolumeID)
	}
	return volumeState{}, false
}

// loadStates skips unreadable files, so one of them cannot keep every other volume from being repaired.
func loadStates() []volumeState {
	entries, err := os.ReadDir(StateDir)
	if err != nil {
		klog.Errorf("reading %s: %v", StateDir, err)
		return nil
	}
	var states []volumeState
	for _, e := range entries {
		st, err := readState(filepath.Join(StateDir, e.Name(), "state.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			klog.Errorf("skipping state of %s: %v", e.Name(), err)
			continue
		}
		states = append(states, st)
	}
	return states
}

// Mount starts rclone for a volume at its staging path.
func Mount(ctx context.Context, volumeID string, remote string, remotePath string, stagingPath string, configData string, flags map[string]string, readOnly bool) error {
	dir := volumeDir(volumeID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	defaultFlags := map[string]string{}
	defaultFlags["cache-info-age"] = "72h"
	defaultFlags["cache-chunk-clean-interval"] = "15m"
	defaultFlags["dir-cache-time"] = "5s"
	defaultFlags["vfs-cache-mode"] = "writes"
	defaultFlags["cache-dir"] = filepath.Join(dir, "cache")
	defaultFlags["allow-non-empty"] = "true"
	defaultFlags["allow-other"] = "true"

	remoteWithPath := fmt.Sprintf(":%s:%s", remote, remotePath)

	if strings.Contains(configData, "["+remote+"]") {
		remoteWithPath = fmt.Sprintf("%s:%s", remote, remotePath)
		klog.V(4).Infof("remote %s found in configData, remoteWithPath set to %s", remote, remoteWithPath)
	}

	// rclone mount remote:path /path/to/mountpoint [flags]
	st := volumeState{VolumeID: volumeID, StagingPath: stagingPath}
	st.Args = []string{
		"mount",
		remoteWithPath,
		stagingPath,
		"--rc",
		"--rc-addr=unix://" + rcSocket(volumeID),
		"--log-file=" + filepath.Join(dir, "rclone.log"),
		"--log-file-max-size=10M",
		"--log-file-max-backups=1",
	}

	// Command line flags take precedence over the RCLONE_* environment set from flags
	if readOnly {
		st.Args = append(st.Args, "--read-only")
	}

	// rclone writes refreshed tokens back into the config, so it stays until NodeUnstageVolume
	if configData != "" {
		configFile := filepath.Join(dir, "rclone.conf")
		if err := os.WriteFile(configFile, []byte(configData), 0600); err != nil {
			return err
		}
		st.Args = append(st.Args, "--config", configFile)
	} else {
		// Disable "config not found" notice
		st.Args = append(st.Args, "--config=")
	}

	// Add default flags
	for k, v := range defaultFlags {
		// Exclude overriden flags
		if _, ok := flags[k]; !ok {
			st.Env = append(st.Env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
		}
	}

	// Add user supplied flags
	for k, v := range flags {
		st.Env = append(st.Env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
	}

	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return err
	}

	// Saved before rclone starts: a crash in between leaves state for a mount
	// that never came up, which reconcile ignores. The reverse order would leave
	// a mount nothing knows how to repair.
	if err := saveState(st); err != nil {
		return err
	}
	if err := startRclone(ctx, st); err != nil {
		_ = os.Remove(statePath(volumeID))
		return err
	}
	st.Mounted = true
	return saveState(st)
}

const mountWaitTimeout = time.Minute

// startRclone returns once rclone serves the staging path.
func startRclone(ctx context.Context, st volumeState) error {
	logFile := filepath.Join(volumeDir(st.VolumeID), "rclone.log")
	var logOffset int64
	if fi, err := os.Stat(logFile); err == nil {
		logOffset = fi.Size()
	}

	// Left over if rclone was killed
	_ = os.Remove(rcSocket(st.VolumeID))

	klog.V(4).Infof("executing rclone %v", st.Args)
	cmd := exec.Command("rclone", st.Args...)
	cmd.Env = append(os.Environ(), st.Env...)
	// rclone outlives the plugin, so it gets its own session and no pipes to
	// the plugin: stdio is /dev/null, since writing to a closed pipe kills it with SIGPIPE.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting rclone: %w", err)
	}
	// Reaped only once adopted, so the scope cannot take a reused pid
	if useSystemd() {
		if err := adoptIntoScope(ctx, scopeName(st.VolumeID), cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
	}
	exited := make(chan struct{})
	go func() {
		err := cmd.Wait()
		klog.V(4).Infof("rclone for volume %s exited: %v", st.VolumeID, err)
		close(exited)
		requestReconcile()
	}()

	if err := waitMounted(ctx, st.StagingPath, exited); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("mounting failed: %w, rclone log: %q", err, readLog(logFile, logOffset))
	}
	klog.V(4).Infof("mounted volume %s at %s (pid %d)", st.VolumeID, st.StagingPath, cmd.Process.Pid)
	return nil
}

func waitMounted(ctx context.Context, path string, exited <-chan struct{}) error {
	deadline := time.NewTimer(mountWaitTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for !isFUSEMount(path) {
		select {
		case <-exited:
			return fmt.Errorf("rclone exited before mounting %s", path)
		case <-deadline.C:
			return fmt.Errorf("%s was not mounted within %s", path, mountWaitTimeout)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
	return nil
}

// readLog returns the end of what rclone logged from offset on.
func readLog(path string, offset int64) string {
	const max = 2048
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size()-offset > max {
		offset = fi.Size() - max
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, _ := io.ReadAll(f)
	return strings.TrimSpace(string(data))
}

type mountState int

const (
	mountOther mountState = iota
	mountLive
	mountDead
	mountGone
)

const fuseSuperMagic = 0x65735546

func classifyMount(fsType int64, statfsErr error) mountState {
	switch {
	case statfsErr == nil && fsType == fuseSuperMagic:
		return mountLive
	case errors.Is(statfsErr, syscall.ENOTCONN):
		return mountDead
	case errors.Is(statfsErr, fs.ErrNotExist):
		return mountGone
	default:
		return mountOther
	}
}

func isFUSEMount(path string) bool {
	return mountStatus(path) == mountLive
}

// https://rclone.org/rc/#core-stats
type rcCoreStatsResponse struct {
	// an array of currently active file transfers
	Transferring map[string]interface{} `json:"transferring"`
}

// https://rclone.org/rc/#vfs-stats
type rcVfsStatsResponse struct {
	DiskCache struct {
		UploadsInProgress int64 `json:"uploadsInProgress"`
		UploadsQueued     int64 `json:"uploadsQueued"`
	} `json:"diskCache"`
}

// waitForUploads blocks until rclone has no transfers or queued uploads left, for at most an hour.
func waitForUploads(volumeID string) {
	socket := rcSocket(volumeID)
	if _, err := os.Stat(socket); err != nil {
		return
	}

	for deadline := time.Now().Add(time.Hour); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
		out, err := RcloneRPC(socket, "core/stats", "{}")
		if err == nil {
			var coreStats rcCoreStatsResponse
			if json.Unmarshal([]byte(out), &coreStats) == nil && len(coreStats.Transferring) > 0 {
				continue
			}
		}

		out, err = RcloneRPC(socket, "vfs/stats", "{}")
		if err == nil {
			var vfsStats rcVfsStatsResponse
			if json.Unmarshal([]byte(out), &vfsStats) == nil &&
				(vfsStats.DiskCache.UploadsInProgress > 0 || vfsStats.DiskCache.UploadsQueued > 0) {
				continue
			}
		}

		return
	}
	klog.Warningf("volume %s still has uploads pending after an hour, unmounting anyway", volumeID)
}

// RcloneRPC calls the rclone rc server listening on socket
func RcloneRPC(socket string, method string, input string) (output string, err error) {
	req, err := http.NewRequest("POST", "http://rclone/"+method, strings.NewReader(input))
	if err != nil {
		return "", fmt.Errorf("cannot create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("cannot read HTTP response: %w", err)
	}
	return string(body), nil
}
