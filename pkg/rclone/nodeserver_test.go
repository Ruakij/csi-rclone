package rclone

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestIsReadOnly(t *testing.T) {
	capability := func(mode csi.VolumeCapability_AccessMode_Mode, flags ...string) *csi.VolumeCapability {
		return &csi.VolumeCapability{
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{MountFlags: flags}},
		}
	}
	rwx := csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	for _, tc := range []struct {
		capability *csi.VolumeCapability
		want       bool
	}{
		{capability(rwx), false},
		{capability(rwx, "noatime"), false},
		{capability(rwx, "ro"), true},
		{capability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY), true},
		{capability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY), true},
	} {
		if got := isReadOnly(tc.capability); got != tc.want {
			t.Errorf("isReadOnly(%v) = %v, want %v", tc.capability, got, tc.want)
		}
	}
}

func TestExtractFlagsSkipsKubernetesKeys(t *testing.T) {
	_, _, _, flags, err := extractFlags(map[string]string{
		"remote": "s3", "remotePath": "bucket",
		"storage.kubernetes.io/csiProvisionerIdentity": "1-csi-rclone",
	}, nil)
	if err != nil || len(flags) != 0 {
		t.Errorf("extractFlags() = %v, %v", flags, err)
	}
}

func TestMountID(t *testing.T) {
	id := func(volumeID string, context, secrets map[string]string) string {
		t.Helper()
		a, err := ephemeralArgs(&csi.NodePublishVolumeRequest{VolumeContext: context, Secrets: secrets, VolumeCapability: &csi.VolumeCapability{}})
		if err != nil {
			t.Fatal(err)
		}
		return mountID(volumeID, a)
	}
	base := map[string]string{"remote": "s3", "remotePath": "bucket", ephemeralContextKey: "true", "csi.storage.k8s.io/pod.name": "a"}
	creds := map[string]string{"s3-access-key-id": "key"}
	shared := id("a", base, creds)
	if got := id("b", map[string]string{"remote": "s3", "remotePath": "bucket", "s3-access-key-id": "key"}, nil); got != shared {
		t.Errorf("same arguments: %s != %s", got, shared)
	}
	if got := id("a", base, map[string]string{"s3-access-key-id": "other"}); got == shared {
		t.Error("different credentials share the mount")
	}
	if got := id("a", map[string]string{"remote": "s3", "remotePath": "other"}, creds); got == shared {
		t.Error("different remotePath shares the mount")
	}

	ReuseMounts = false
	t.Cleanup(func() { ReuseMounts = true })
	if got := id("a", base, creds); got != "a" {
		t.Errorf("unshared ID = %s, want a", got)
	}
}
