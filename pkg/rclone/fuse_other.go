//go:build !linux

package rclone

import "errors"

var errNotLinux = errors.New("mounting is only supported on linux")

func mountStatus(string) mountState        { return mountOther }
func lazyUnmount(string) error             { return errNotLinux }
func bindMount(string, string, bool) error { return errNotLinux }
