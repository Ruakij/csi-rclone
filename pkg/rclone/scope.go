package rclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	systemd "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
	"k8s.io/klog/v2"
)

// DaemonLifetime is auto, systemd or in-container.
var DaemonLifetime = "auto"

// ScopeMemoryMax and ScopeTasksMax limit each rclone scope, 0 means no limit.
var ScopeMemoryMax, ScopeTasksMax uint64

const (
	scopePrefix       = "csi-rclone-"
	scopeSuffix       = ".scope"
	reconcileInterval = 30 * time.Second
	minReconcileGap   = time.Second
)

func scopeName(volumeID string) string {
	return scopePrefix + filepath.Base(volumeDir(volumeID)) + scopeSuffix
}

// useSystemd reports whether rclone processes are moved into host systemd
// scopes, which lets them outlive the plugin container.
var useSystemd = sync.OnceValue(func() bool {
	if DaemonLifetime == "in-container" {
		return false
	}
	conn, err := systemd.NewSystemdConnectionContext(context.Background())
	if err != nil {
		if DaemonLifetime == "systemd" {
			klog.Fatalf("host systemd is unreachable: %v", err)
		}
		klog.Warningf("host systemd is unreachable (%v): rclone mounts die with this pod and are remounted when it restarts, "+
			"files open across a restart return ENOTCONN until reopened", err)
		return false
	}
	conn.Close()
	return true
})

// adoptIntoScope moves a running process into a transient host systemd scope,
// like systemd-run --scope. The container runtime then no longer kills it with the plugin.
func adoptIntoScope(ctx context.Context, unit string, pid int) error {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connecting to host systemd: %w", err)
	}
	defer conn.Close()

	props := []systemd.Property{
		systemd.PropDescription("csi-rclone mount"),
		{Name: "PIDs", Value: godbus.MakeVariant([]uint32{uint32(pid)})},
		{Name: "Delegate", Value: godbus.MakeVariant(true)},
		// Removes the unit once rclone exits, so the volume can reuse the name
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive-or-failed")},
	}
	if ScopeMemoryMax > 0 {
		props = append(props, systemd.Property{Name: "MemoryMax", Value: godbus.MakeVariant(ScopeMemoryMax)})
	}
	if ScopeTasksMax > 0 {
		props = append(props, systemd.Property{Name: "TasksMax", Value: godbus.MakeVariant(ScopeTasksMax)})
	}

	done := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, unit, "replace", props, done); err != nil {
		return fmt.Errorf("starting scope %s: %w", unit, err)
	}
	select {
	case result := <-done:
		if result != "done" {
			return fmt.Errorf("starting scope %s: %s", unit, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stopScope is usually a no-op: rclone exits on unmount, and CollectMode removes the empty scope.
func stopScope(ctx context.Context, volumeID string) {
	if !useSystemd() {
		return
	}
	unit := scopeName(volumeID)
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		klog.Warningf("connecting to host systemd to stop %s: %v", unit, err)
		return
	}
	defer conn.Close()
	if _, err := conn.StopUnitContext(ctx, unit, "replace", nil); err != nil {
		klog.V(4).Infof("stopping %s: %v", unit, err)
	}
}

// wakeReconcile holds at most one pending wake, so bursts of exits collapse into one pass.
var wakeReconcile = make(chan struct{}, 1)

func requestReconcile() {
	select {
	case wakeReconcile <- struct{}{}:
	default:
	}
}

// reconcile remounts volumes whose rclone died, and binds them into their pods again.
// Only the node plugin has StateDir, so the controller skips it.
func reconcile(ctx context.Context) {
	if _, err := os.Stat(StateDir); err != nil {
		return
	}
	if useSystemd() {
		if err := watchScopes(ctx); err != nil {
			klog.Warningf("cannot watch host systemd for rclone exits (%v), remounting within %s instead", err, reconcileInterval)
		}
	}

	tick := time.NewTicker(reconcileInterval)
	defer tick.Stop()
	var last time.Time
	for first := true; ; first = false {
		// An rclone dying right after mounting must not cause a tight remount loop
		if wait := minReconcileGap - time.Since(last); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
		last = time.Now()
		for _, st := range loadStates() {
			reconcileVolume(ctx, st.VolumeID, first)
		}
		select {
		case <-tick.C:
		case <-wakeReconcile:
		case <-ctx.Done():
			return
		}
	}
}

// watchScopes wakes reconcile when a scope stops. rclone processes started by an
// earlier plugin instance are not children of this one, so only systemd sees them exit.
func watchScopes(ctx context.Context) error {
	conn, err := systemd.NewSystemdConnectionContext(ctx)
	if err != nil {
		return err
	}
	updates := make(chan *systemd.PropertiesUpdate, 64)
	dropped := make(chan error, 1)
	conn.SetPropertiesSubscriber(updates, dropped)
	if err := conn.Subscribe(); err != nil {
		conn.Close()
		return err
	}

	go func() {
		defer conn.Close()
		for {
			select {
			case u := <-updates:
				state, ok := u.Changed["ActiveState"]
				if ok && strings.HasPrefix(u.UnitName, scopePrefix) && strings.HasSuffix(u.UnitName, scopeSuffix) && state.Value() != "active" {
					requestReconcile()
				}
			case <-dropped:
				requestReconcile()
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func reconcileVolume(ctx context.Context, volumeID string, logLive bool) {
	lockVolume(volumeID)
	defer unlockVolume(volumeID)

	// Reloaded under the lock, the volume may have been unstaged or published meanwhile
	st, err := loadState(volumeID)
	if err != nil {
		return
	}

	switch s := mountStatus(st.StagingPath); s {
	case mountLive:
		if logLive {
			klog.Infof("found live mount of volume %s at %s", volumeID, st.StagingPath)
		}
	case mountDead, mountOther:
		// rclone unmounts when stopped with SIGTERM, leaving a plain directory
		if s == mountOther && !st.Mounted {
			return
		}
		klog.Warningf("rclone for volume %s died, remounting %s", volumeID, st.StagingPath)
		if err := remount(ctx, st); err != nil {
			klog.Errorf("remounting volume %s: %v", volumeID, err)
			return
		}
	case mountGone:
		// Kubelet removed the staging path 4.0.0 mounted at, the volume is gone
		klog.Infof("removing state of volume %s, %s is gone", volumeID, st.StagingPath)
		stopScope(ctx, volumeID)
		if err := os.RemoveAll(volumeDir(volumeID)); err != nil {
			klog.Errorf("removing state of volume %s: %v", volumeID, err)
		}
		return
	}

	// Bind mounts still point at the dead mount after a remount
	changed := false
	for target, readOnly := range st.Targets {
		switch mountStatus(target) {
		case mountDead:
			klog.Warningf("binding volume %s into %s again", volumeID, target)
			err := lazyUnmount(target)
			if err == nil {
				err = bindMount(st.StagingPath, target, readOnly)
			}
			if err != nil {
				klog.Errorf("binding volume %s into %s again: %v", volumeID, target, err)
			}
		case mountGone:
			delete(st.Targets, target)
			changed = true
		}
	}
	// Kubelet removed them without unstaging or unpublishing, or the plugin
	// crashed between mounting and recording the first user
	for user := range st.Users {
		if mountStatus(user) == mountGone {
			delete(st.Users, user)
			changed = true
		}
	}
	if len(st.Users) == 0 {
		klog.Infof("unmounting volume %s, nothing uses it", volumeID)
		if err := unmount(ctx, volumeID, st.StagingPath); err != nil {
			klog.Errorf("unmounting volume %s: %v", volumeID, err)
		}
		return
	}
	if changed {
		if err := saveState(st); err != nil {
			klog.Errorf("saving state of volume %s: %v", volumeID, err)
		}
	}
}

func remount(ctx context.Context, st volumeState) error {
	if err := lazyUnmount(st.StagingPath); err != nil {
		return err
	}
	stopScope(ctx, st.VolumeID)
	return startRclone(ctx, st)
}
