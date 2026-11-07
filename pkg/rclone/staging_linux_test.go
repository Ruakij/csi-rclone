package rclone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var pvContext = map[string]string{"remote": "s3", "remotePath": "bucket"}

func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
	}
}

func stage(ns *nodeServer, t *testing.T, volumeID, stagingPath string) error {
	t.Helper()
	_, err := ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId: volumeID, StagingTargetPath: stagingPath, VolumeContext: pvContext, VolumeCapability: mountCapability(),
	})
	return err
}

func publishEphemeral(ns *nodeServer, t *testing.T, volumeID, targetPath string) error {
	t.Helper()
	_, err := ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId: volumeID, TargetPath: targetPath, VolumeCapability: mountCapability(),
		VolumeContext: map[string]string{"remote": "s3", "remotePath": "bucket", ephemeralContextKey: "true"},
	})
	return err
}

// rcloneStarts returns the arguments of each rclone start the fake rclone recorded.
func rcloneStarts(t *testing.T, marker string) [][]string {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		return nil
	}
	var starts [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		starts = append(starts, strings.Fields(line))
	}
	return starts
}

// mountPointOf and socketOf read what a recorded start mounts where: "mount remote mountpoint --rc --rc-addr=unix://socket ...".
func mountPointOf(args []string) string { return args[2] }

func socketOf(args []string) string {
	for _, a := range args {
		if s, ok := strings.CutPrefix(a, "--rc-addr=unix://"); ok {
			return s
		}
	}
	return ""
}

// The fake rclone fails, so the calls fail with Internal, but they show where and as which mount rclone was started.
func newStagingFixture(t *testing.T) (*nodeServer, string) {
	t.Helper()
	StateDir = t.TempDir()
	marker := fakeRclone(t)
	return &nodeServer{mounts: newTestMounts(t)}, marker
}

func TestStageMountsPersistentVolumeAtStagingPath(t *testing.T) {
	ns, marker := newStagingFixture(t)
	if !ReuseMounts {
		t.Fatal("ReuseMounts is off, the test needs it on")
	}
	stagingA := filepath.Join(t.TempDir(), "a", "globalmount")
	stagingB := filepath.Join(t.TempDir(), "b", "globalmount")

	// Equal arguments share no mount, with ReuseMounts on
	for volumeID, staging := range map[string]string{"pv-a": stagingA, "pv-b": stagingB} {
		if err := stage(ns, t, volumeID, staging); status.Code(err) != codes.Internal {
			t.Fatalf("stage %s = %v, want Internal from the failing fake rclone", volumeID, err)
		}
	}
	starts := rcloneStarts(t, marker)
	if len(starts) != 2 {
		t.Fatalf("rclone started %d times, want 2: %v", len(starts), starts)
	}
	got := map[string]string{}
	for _, args := range starts {
		got[mountPointOf(args)] = socketOf(args)
	}
	if got[stagingA] != rcSocket("pv-a") || got[stagingB] != rcSocket("pv-b") {
		t.Errorf("mount point -> rc socket = %v, want %s -> %s, %s -> %s", got, stagingA, rcSocket("pv-a"), stagingB, rcSocket("pv-b"))
	}
}

func TestEphemeralVolumesShareMount(t *testing.T) {
	ns, marker := newStagingFixture(t)
	for _, volumeID := range []string{"eph-a", "eph-b"} {
		if err := publishEphemeral(ns, t, volumeID, filepath.Join(t.TempDir(), "target")); status.Code(err) != codes.Internal {
			t.Fatalf("publish %s = %v, want Internal from the failing fake rclone", volumeID, err)
		}
	}
	starts := rcloneStarts(t, marker)
	if len(starts) != 2 {
		t.Fatalf("rclone started %d times, want 2: %v", len(starts), starts)
	}
	if socketOf(starts[0]) == "" || socketOf(starts[0]) != socketOf(starts[1]) || mountPointOf(starts[0]) != mountPointOf(starts[1]) {
		t.Errorf("ephemeral volumes with equal arguments use different mounts: %v", starts)
	}
	if !strings.HasPrefix(mountPointOf(starts[0]), StateDir) || filepath.Base(mountPointOf(starts[0])) != "mnt" {
		t.Errorf("ephemeral mount point %s, want mnt in the state directory", mountPointOf(starts[0]))
	}
}

// oldLayoutState is a mount of 4.1.0 to 4.2.x: rclone at mountPath, shared by the persistent volumes staged at users.
func oldLayoutState(t *testing.T, id string, users ...string) volumeState {
	t.Helper()
	st := volumeState{VolumeID: id, StagingPath: mountPath(id), Mounted: true, Args: []string{"mount"}, Users: map[string]struct{}{}}
	for _, u := range users {
		st.Users[u] = struct{}{}
	}
	if err := os.MkdirAll(mountPath(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveState(st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestOldLayoutIsUnstagedUserByUser(t *testing.T) {
	ns, _ := newStagingFixture(t)
	stagingA, stagingB := filepath.Join(t.TempDir(), "globalmount"), filepath.Join(t.TempDir(), "globalmount")
	st := oldLayoutState(t, "shared", stagingA, stagingB)

	// Publishing binds from the rclone mount, which is not live here
	_, err := ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId: "pv-a", StagingTargetPath: stagingA, TargetPath: filepath.Join(t.TempDir(), "target"), VolumeCapability: mountCapability(),
	})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), mountPath(st.VolumeID)) {
		t.Errorf("publish = %v, want FailedPrecondition naming %s", err, mountPath(st.VolumeID))
	}

	unstage := func(volumeID, staging string) {
		t.Helper()
		if _, err := ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{VolumeId: volumeID, StagingTargetPath: staging}); err != nil {
			t.Fatal(err)
		}
	}
	unstage("pv-a", stagingA)
	got, err := loadState(st.VolumeID)
	if err != nil || len(got.Users) != 1 || !got.uses(stagingB) || got.StagingPath != mountPath(st.VolumeID) {
		t.Fatalf("state after the first unstage = %+v, %v", got, err)
	}
	unstage("pv-b", stagingB)
	if _, err := os.Stat(volumeDir(st.VolumeID)); !os.IsNotExist(err) {
		t.Errorf("state directory after the last unstage: %v", err)
	}
}

func TestStageDoesNotJoinOldLayoutMount(t *testing.T) {
	ns, marker := newStagingFixture(t)
	a, err := ephemeralArgs(&csi.NodePublishVolumeRequest{VolumeContext: pvContext, VolumeCapability: mountCapability()})
	if err != nil {
		t.Fatal(err)
	}
	// What 4.1.0 to 4.2.x named the shared mount of a persistent volume with these arguments
	shared := oldLayoutState(t, mountID("pv-old", a), filepath.Join(t.TempDir(), "globalmount"))

	staging := filepath.Join(t.TempDir(), "globalmount")
	if err := stage(ns, t, "pv-new", staging); status.Code(err) != codes.Internal {
		t.Fatalf("stage = %v, want Internal from the failing fake rclone", err)
	}
	starts := rcloneStarts(t, marker)
	if len(starts) != 1 || mountPointOf(starts[0]) != staging || socketOf(starts[0]) != rcSocket("pv-new") {
		t.Errorf("rclone starts = %v, want one at %s for pv-new", starts, staging)
	}
	got, err := loadState(shared.VolumeID)
	if err != nil || len(got.Users) != 1 {
		t.Errorf("old-layout state after staging = %+v, %v", got, err)
	}
}
