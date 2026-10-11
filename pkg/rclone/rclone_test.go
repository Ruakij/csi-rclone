package rclone

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/supervisor"
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
		Users: map[string]struct{}{"/staging": {}}, Targets: map[string]bool{"/t": true}}
	if err := saveState(want); err != nil {
		t.Fatal(err)
	}
	if states := loadStates(); len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Errorf("loadStates() = %+v, want [%+v]", states, want)
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
