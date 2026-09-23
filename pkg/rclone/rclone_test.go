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
	want := volumeState{VolumeID: id, StagingPath: "/staging", Args: []string{"mount", "a:b"}, Env: []string{"RCLONE_X=1"}}
	if err := saveState(want); err != nil {
		t.Fatal(err)
	}

	ro, rw := true, false
	for _, step := range []struct {
		target   string
		readOnly *bool
	}{{"/t1", &ro}, {"/t2", &rw}, {"/t1", nil}, {"/unknown", nil}} {
		if err := updateTargets(id, step.target, step.readOnly); err != nil {
			t.Fatal(err)
		}
	}
	want.Targets = map[string]bool{"/t2": false}

	if states := loadStates(); len(states) != 1 || !reflect.DeepEqual(states[0], want) {
		t.Errorf("loadStates() = %+v, want [%+v]", states, want)
	}
	if fi, err := os.Stat(statePath(id)); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("state file: %v, %v", fi, err)
	}
	if err := updateTargets("unstaged", "/t", &ro); !os.IsNotExist(err) {
		t.Errorf("updateTargets on unstaged volume: %v", err)
	}
}

func TestScopeName(t *testing.T) {
	name := scopeName("pvc-1/../x y")
	if !regexp.MustCompile(`^csi-rclone-[0-9a-f]{16}\.scope$`).MatchString(name) || name != scopeName("pvc-1/../x y") || name == scopeName("pvc-2") {
		t.Errorf("scopeName = %q", name)
	}
}
