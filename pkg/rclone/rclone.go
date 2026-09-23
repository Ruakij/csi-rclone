package rclone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/utils/keymutex"
)

// StateDir holds a directory per staged volume with its rclone config, rc socket and VFS cache.
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

// volumeLocks serializes all node calls per volume. Kubelet retries a slow
// stage while the first one is still mounting.
var volumeLocks = keymutex.NewHashed(0)

func lockVolume(volumeID string)   { volumeLocks.LockKey(volumeID) }
func unlockVolume(volumeID string) { _ = volumeLocks.UnlockKey(volumeID) }

// Mount starts rclone for a volume at its staging path.
func Mount(ctx context.Context, volumeID string, remote string, remotePath string, stagingPath string, configData string, flags map[string]string, readOnly bool) error {
	dir := volumeDir(volumeID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// Left over if a previous rclone for this volume died
	os.Remove(rcSocket(volumeID))

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
	mountArgs := []string{
		"mount",
		remoteWithPath,
		stagingPath,
		"--rc",
		"--rc-addr=unix://" + rcSocket(volumeID),
		"--daemon",
		"--daemon-wait=0",
	}

	// Command line flags take precedence over the RCLONE_* environment set from flags
	if readOnly {
		mountArgs = append(mountArgs, "--read-only")
	}

	// rclone reads the config after forking and writes refreshed tokens back,
	// so the file stays until NodeUnstageVolume
	if configData != "" {
		configFile := filepath.Join(dir, "rclone.conf")
		if err := os.WriteFile(configFile, []byte(configData), 0600); err != nil {
			return err
		}
		mountArgs = append(mountArgs, "--config", configFile)
	} else {
		// Disable "config not found" notice
		mountArgs = append(mountArgs, "--config=")
	}

	env := os.Environ()

	// Add default flags
	for k, v := range defaultFlags {
		// Exclude overriden flags
		if _, ok := flags[k]; !ok {
			env = append(env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
		}
	}

	// Add user supplied flags
	for k, v := range flags {
		env = append(env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
	}

	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return err
	}

	klog.V(4).Infof("executing rclone %v", mountArgs)

	cmd := exec.CommandContext(ctx, "rclone", mountArgs...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mounting failed: %v remote: '%s' path: %s output: %q", err, remoteWithPath, stagingPath, string(out))
	}

	return nil
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
		return "", fmt.Errorf("cannot create HTTP request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot send HTTP request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("cannot read HTTP response: %v", err)
	}
	return string(body), nil
}
