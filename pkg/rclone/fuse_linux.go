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

// bindMount makes a read-only bind read-only before attaching it. A remount
// afterwards would only change it in this mount namespace, not in the host's.
func bindMount(source, target string, readOnly bool) error {
	if !readOnly {
		if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s to %s: %w", source, target, err)
		}
		return nil
	}
	fd, err := unix.OpenTree(unix.AT_FDCWD, source, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone %s: %w", source, err)
	}
	defer unix.Close(fd)
	attr := &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV}
	if err := unix.MountSetattr(fd, "", unix.AT_EMPTY_PATH, attr); err != nil {
		return fmt.Errorf("make %s read-only (needs Linux 5.12): %w", source, err)
	}
	if err := unix.MoveMount(fd, "", unix.AT_FDCWD, target, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("bind %s to %s: %w", source, target, err)
	}
	return nil
}
