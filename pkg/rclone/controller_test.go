package rclone

import "testing"

func TestValidateRemotePathSuffix(t *testing.T) {
	meta := &pvcMetadata{
		data:        map[string]string{"name": "data", "namespace": "tenant-a"},
		annotations: map[string]string{"csi-rclone/storage-path": "../tenant-b/data"},
	}
	if err := validateRemotePathSuffix(meta.stringParser("${.PVC.namespace}/${.PVC.annotations.csi-rclone/storage-path}")); err == nil {
		t.Error("traversal via annotation accepted")
	}
	for _, bad := range []string{"..", "a/..", "a/../b", "./a", "a/./b"} {
		if validateRemotePathSuffix(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"tenant-a/data", "/tenant-a", "tenant-a/..data", "tenant-a/.hidden", "a//b"} {
		if err := validateRemotePathSuffix(good); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
}
