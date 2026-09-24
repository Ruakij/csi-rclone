//go:build linux

package rclone

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// mountStatus uses statfs, which unlike stat is never answered from the attribute cache.
func mountStatus(path string) mountState {
	var st unix.Statfs_t
	err := unix.Statfs(path, &st)
	return classifyMount(st.Type, err)
}

// lazyUnmount also succeeds when rclone is dead or files are still open, and when path is not mounted.
func lazyUnmount(path string) error {
	switch err := unix.Unmount(path, unix.MNT_DETACH); err {
	case nil, unix.EINVAL, unix.ENOENT:
		return nil
	case unix.EPERM:
		// Unprivileged callers get EPERM for paths that are not mounted, too
		if s := mountStatus(path); s != mountLive && s != mountDead {
			return nil
		}
		return fmt.Errorf("unmount %s: %w", path, err)
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
