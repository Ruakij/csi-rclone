//go:build linux

package rclone

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const fuseSuperMagic = 0x65735546

// isFUSEMount is false for a dead FUSE mount, whose statfs fails with ENOTCONN.
func isFUSEMount(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	return st.Type == fuseSuperMagic
}

// lazyUnmount also succeeds when rclone is dead or files are still open, and when path is not mounted.
func lazyUnmount(path string) error {
	switch err := unix.Unmount(path, unix.MNT_DETACH); err {
	case nil, unix.EINVAL, unix.ENOENT:
		return nil
	default:
		return fmt.Errorf("unmount %s: %w", path, err)
	}
}

func bindMount(source, target string, readOnly bool) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s to %s: %w", source, target, err)
	}
	if !readOnly {
		return nil
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV)
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		_ = lazyUnmount(target)
		return fmt.Errorf("remount %s read-only: %w", target, err)
	}
	return nil
}
