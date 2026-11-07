package rclone

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/supervisor"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inContainer keeps rclone in the process of the test, which has no host systemd.
func inContainer(t *testing.T) {
	t.Helper()
	old := DaemonLifetime
	DaemonLifetime = "in-container"
	t.Cleanup(func() { DaemonLifetime = old })
}

func newTestMounts(t *testing.T) *mounts {
	t.Helper()
	inContainer(t)
	m, err := newMounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestState(t *testing.T) {
	StateDir = t.TempDir()
	const id = "pvc-1/with:odd chars"
	if err := os.MkdirAll(volumeDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	want := volumeState{VolumeID: id, StagingPath: mountPath(id), Args: []string{"mount", "a:b"}, Env: []string{"RCLONE_X=1"},
		Users: map[string]struct{}{"/staging": {}}, Targets: map[string]bool{"/t": true},
		DriverVersion: "v4.3.0", StartedAt: time.Now().Add(-90 * time.Minute).UTC()}
	if err := saveState(want); err != nil {
		t.Fatal(err)
	}
	if states := loadStates(); len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Errorf("loadStates() = %+v, want [%+v]", states, want)
	}
	if got, want := want.origin(), "started 1h30m0s ago by driver v4.3.0"; got != want {
		t.Errorf("origin() = %q, want %q", got, want)
	}
	if fi, err := os.Stat(statePath(id)); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("state file: %v, %v", fi, err)
	}

	m := newTestMounts(t)
	for _, path := range []string{"/staging", "/t"} {
		st, ok := m.lockMountOf(path)
		if !ok || st.VolumeID != id {
			t.Fatalf("lockMountOf(%s) = %v, %v", path, st.VolumeID, ok)
		}
		m.sup.Unlock(id)
	}
	if _, ok := m.lockMountOf("/other"); ok {
		t.Error("lockMountOf(/other) found a mount")
	}
}

func TestLegacyState(t *testing.T) {
	StateDir = t.TempDir()
	if err := os.MkdirAll(volumeDir("pv"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath("pv"), []byte(`{"volumeID":"pv","stagingPath":"/kubelet/globalmount"}`), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := loadState("pv")
	if _, ok := st.Users["/kubelet/globalmount"]; err != nil || !ok || len(st.Users) != 1 {
		t.Errorf("4.0.0 state users = %v, %v", st.Users, err)
	}
}

// The names of scopes and state files are how a new node plugin finds the rclone
// processes of the old one, so they are pinned to what earlier releases wrote: the
// reference functions and the literals below are theirs.
var compatIDs = map[string]string{
	"pv":                   "0717031b935f131e",
	"pvc-1/with:odd chars": "487013acf7c4cc7e",
	"e2e-shared":           "513b83f92617d9f4",
	"0b8e5c7a9d3f41e2a6b0c8d7e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6": "d9bfa789cf39e28e",
	"": "e3b0c44298fc1c14",
}

func legacyVolumeDir(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return filepath.Join(StateDir, hex.EncodeToString(sum[:8]))
}

func legacyScopeName(volumeID string) string {
	return "csi-rclone-" + filepath.Base(legacyVolumeDir(volumeID)) + ".scope"
}

func legacyStatePath(volumeID string) string {
	return filepath.Join(legacyVolumeDir(volumeID), "state.json")
}

func TestScopeNameCompat(t *testing.T) {
	StateDir = t.TempDir()
	cfg := scopeConfig(scope.InContainer)
	m, err := scope.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for id, hash := range compatIDs {
		got, err := m.Unit(id)
		want := "csi-rclone-" + hash + ".scope"
		if err != nil || got != want || got != legacyScopeName(id) {
			t.Errorf("scope of %q = %q, %v; want %q, legacy %q", id, got, err, want, legacyScopeName(id))
		}
	}
	if cfg.Description != "csi-rclone mount" {
		t.Errorf("scope description = %q", cfg.Description)
	}
}

func TestStateLayoutCompat(t *testing.T) {
	StateDir = t.TempDir()
	store := stateStore()
	for id, hash := range compatIDs {
		if got, want := store.Path(id), filepath.Join(StateDir, hash, "state.json"); got != want || got != legacyStatePath(id) {
			t.Errorf("state path of %q = %q, want %q, legacy %q", id, got, want, legacyStatePath(id))
		}
	}

	// A state file written the way earlier releases wrote it is found by the glob.
	const id = "pvc-1/with:odd chars"
	if err := os.MkdirAll(legacyVolumeDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	data := `{"volumeID":"` + id + `","stagingPath":"/kubelet/globalmount","users":{"/kubelet/globalmount":{}},"mounted":true}`
	if err := os.WriteFile(legacyStatePath(id), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if states := loadStates(); len(states) != 1 || states[0].VolumeID != id || !states[0].Mounted {
		t.Errorf("loadStates() = %+v", states)
	}
}

// A mount point that is not mounted is repaired only once rclone is gone: an rclone that
// runs may serve pods through copies of its mount that a new mount would not reach.
func TestCheckLeavesRunningRcloneAlone(t *testing.T) {
	StateDir = t.TempDir()
	m := newTestMounts(t)
	mountPoint := t.TempDir()
	cmd := exec.Command("sleep", "60")
	d, err := m.sup.Start(t.Context(), "pv", cmd, mountPoint, supervisor.StartOptions{Ready: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	st := volumeState{VolumeID: "pv", StagingPath: mountPoint, Mounted: true}

	if _, err := m.check(st); err == nil {
		t.Error("check of an unmounted mount point of a running rclone: no error, it would be replaced")
	}
	// A mount that never came up has no rclone serving it
	if _, err := m.check(volumeState{VolumeID: "pv", StagingPath: mountPoint}); err != nil {
		t.Errorf("check of a mount that never came up: %v", err)
	}

	_ = cmd.Process.Kill()
	select {
	case <-d.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("rclone did not exit")
	}
	if _, err := m.check(st); err != nil {
		t.Errorf("check after rclone exited: %v", err)
	}
}

// fakeRclone stands in for rclone: it records the arguments of each start as a line of the returned file and exits at once.
func fakeRclone(t *testing.T) string {
	t.Helper()
	dir, marker := t.TempDir(), filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// deadMountState is a volume whose rclone is gone: the staging path is a plain directory although the state says it was mounted.
func deadMountState(t *testing.T) volumeState {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("fusemount tells mount states apart on Linux only")
	}
	StateDir = t.TempDir()
	staging := t.TempDir()
	st := volumeState{VolumeID: "pv", StagingPath: staging, Mounted: true, Args: []string{"mount"},
		Users: map[string]struct{}{staging: {}, "/gone/user": {}}, Targets: map[string]bool{"/gone/target": false}}
	if err := os.MkdirAll(volumeDir(st.VolumeID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	return st
}

// Only staging the volume again replaces a mount whose rclone died, the reconciler
// forgets what kubelet removed and leaves the rest.
func TestReconcileLeavesDeadMountAlone(t *testing.T) {
	st := deadMountState(t)
	marker := fakeRclone(t)
	m := newTestMounts(t)

	m.reconcile(t.Context(), st.VolumeID, false)

	if _, err := os.Stat(marker); err == nil {
		t.Error("reconcile started rclone for a dead mount")
	}
	got, err := loadState(st.VolumeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Users[st.StagingPath]; !ok || len(got.Users) != 1 || len(got.Targets) != 0 || !got.Mounted {
		t.Errorf("state after reconcile = %+v", got)
	}
}

func TestEnsureLiveRemountsDeadMount(t *testing.T) {
	st := deadMountState(t)
	marker := fakeRclone(t)
	m := newTestMounts(t)

	// The fake rclone exits at once, so the remount fails, but it was tried
	err := m.ensureLive(t.Context(), &st)
	if status.Code(err) != codes.Internal {
		t.Errorf("ensureLive() = %v, want Internal", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("ensureLive did not start rclone for a dead mount")
	}
}

// NodeUnstageVolume of a dead mount drains uploads first, and the rc socket of a killed rclone stays behind.
func TestWaitForUploadsReturnsForDeadRclone(t *testing.T) {
	// Short, as the path of a unix socket is limited to about 100 bytes
	dir, err := os.MkdirTemp("/tmp", "rc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	StateDir = dir
	if err := os.MkdirAll(volumeDir("pv"), 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", rcSocket("pv"))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()

	done := make(chan struct{})
	go func() { waitForUploads("pv"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForUploads hangs on a socket nobody listens on")
	}
}
