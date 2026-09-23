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
		req  *csi.NodePublishVolumeRequest
		want bool
	}{
		{&csi.NodePublishVolumeRequest{VolumeCapability: capability(rwx)}, false},
		{&csi.NodePublishVolumeRequest{VolumeCapability: capability(rwx, "noatime")}, false},
		{&csi.NodePublishVolumeRequest{VolumeCapability: capability(rwx), Readonly: true}, true},
		{&csi.NodePublishVolumeRequest{VolumeCapability: capability(rwx, "ro")}, true},
		{&csi.NodePublishVolumeRequest{VolumeCapability: capability(csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY)}, true},
		{&csi.NodePublishVolumeRequest{VolumeCapability: capability(csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY)}, true},
	} {
		if got := isReadOnly(tc.req); got != tc.want {
			t.Errorf("isReadOnly(%v) = %v, want %v", tc.req, got, tc.want)
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
