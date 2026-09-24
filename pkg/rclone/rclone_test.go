package rclone

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"syscall"
	"testing"
)

func TestClassifyMount(t *testing.T) {
	for _, tc := range []struct {
		fsType int64
		err    error
		want   mountState
	}{
		{fuseSuperMagic, nil, mountLive},
		{0xEF53, nil, mountOther},
		{0, syscall.ENOTCONN, mountDead},
		{0, syscall.ENOENT, mountGone},
		{0, syscall.EACCES, mountOther},
		{fuseSuperMagic, errors.New("other"), mountOther},
	} {
		if got := classifyMount(tc.fsType, tc.err); got != tc.want {
			t.Errorf("classifyMount(%#x, %v) = %v, want %v", tc.fsType, tc.err, got, tc.want)
		}
	}
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

	for _, path := range []string{"/staging", "/t"} {
		st, ok := lockMountOf(path)
		if !ok || st.VolumeID != id {
			t.Fatalf("lockMountOf(%s) = %v, %v", path, st.VolumeID, ok)
		}
		unlockVolume(id)
	}
	if _, ok := lockMountOf("/other"); ok {
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

func TestScopeName(t *testing.T) {
	name := scopeName("pvc-1/../x y")
	if !regexp.MustCompile(`^csi-rclone-[0-9a-f]{16}\.scope$`).MatchString(name) || name != scopeName("pvc-1/../x y") || name == scopeName("pvc-2") {
		t.Errorf("scopeName = %q", name)
	}
}
