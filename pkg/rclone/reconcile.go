package rclone

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/fusemount"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/scope"
	"github.com/Ruakij/go-csi-fuse-supervisor/pkg/supervisor"
	"k8s.io/klog/v2"
)

// DaemonLifetime is auto, systemd or in-container.
var DaemonLifetime = "auto"

// ScopeMemoryMax and ScopeTasksMax limit each rclone scope, 0 means no limit.
var ScopeMemoryMax, ScopeTasksMax uint64

const scopePrefix = "csi-rclone-"

// scopeConfig names the scope of a volume after its state directory. A node
// plugin finds the rclone processes of an earlier instance by these names, so
// they stay as they are.
func scopeConfig(lifetime scope.Lifetime) scope.Config {
	return scope.Config{
		Prefix:      scopePrefix,
		Description: "csi-rclone mount",
		Lifetime:    lifetime,
		MemoryMax:   ScopeMemoryMax,
		TasksMax:    ScopeTasksMax,
		Unit:        func(volumeID string) string { return scopePrefix + volumeDirName(volumeID) + ".scope" },
	}
}

// mounts starts the rclone processes of the node, repairs the mounts of those that died,
// and serializes both with the CSI calls per mount.
type mounts struct {
	scopes *scope.Manager
	sup    *supervisor.Supervisor
}

func newMounts(ctx context.Context) (*mounts, error) {
	lifetime, err := scope.ParseLifetime(DaemonLifetime)
	if err != nil {
		return nil, err
	}
	scopes, err := scope.New(ctx, scopeConfig(lifetime))
	if err != nil {
		return nil, err
	}
	m := &mounts{scopes: scopes}
	m.sup, err = supervisor.New(supervisor.Config{
		Scope: scopes,
		IDs: func() ([]string, error) {
			var ids []string
			for _, st := range loadStates() {
				ids = append(ids, st.VolumeID)
			}
			return ids, nil
		},
		Reconcile: m.reconcile,
		OnError:   func(err error) { klog.Error(err) },
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// run repairs mounts until ctx is done. Only the node plugin has StateDir, so the controller skips it.
func (m *mounts) run(ctx context.Context) {
	if _, err := os.Stat(StateDir); err != nil {
		return
	}
	if err := m.scopes.FellBack(); err != nil {
		klog.Warningf("host systemd is unreachable (%v): rclone mounts die with this pod and are remounted when it restarts, "+
			"files open across a restart return ENOTCONN until reopened", err)
	}
	m.sup.Run(ctx)
}

// stopScope is usually a no-op: rclone exits on unmount, and the empty scope is collected with it.
func (m *mounts) stopScope(ctx context.Context, volumeID string) {
	if err := m.scopes.Stop(ctx, volumeID); err != nil {
		klog.Warningf("stopping the scope of volume %s: %v", volumeID, err)
	}
}

// reconcile remounts a volume whose rclone died, and binds it into its pods again. The caller holds the lock.
func (m *mounts) reconcile(ctx context.Context, volumeID string, first bool) {
	// Reloaded under the lock, the volume may have been unstaged or published meanwhile
	st, err := loadState(volumeID)
	if err != nil {
		return
	}

	switch s := fusemount.Status(st.StagingPath); s {
	case fusemount.Live:
		if first {
			klog.Infof("found live mount of volume %s at %s", volumeID, st.StagingPath)
		}
	case fusemount.Dead, fusemount.Other:
		// rclone unmounts when stopped with SIGTERM, leaving a plain directory
		if s == fusemount.Other && !st.Mounted {
			return
		}
		klog.Warningf("rclone for volume %s died, remounting %s", volumeID, st.StagingPath)
		if err := m.remount(ctx, st); err != nil {
			klog.Errorf("remounting volume %s: %v", volumeID, err)
			return
		}
	case fusemount.Gone:
		// Kubelet removed the staging path 4.0.0 mounted at, the volume is gone
		klog.Infof("removing state of volume %s, %s is gone", volumeID, st.StagingPath)
		m.stopScope(ctx, volumeID)
		if err := os.RemoveAll(volumeDir(volumeID)); err != nil {
			klog.Errorf("removing state of volume %s: %v", volumeID, err)
		}
		return
	}

	// Bind mounts still point at the dead mount after a remount
	changed := false
	for target, readOnly := range st.Targets {
		switch fusemount.Status(target) {
		case fusemount.Dead:
			klog.Warningf("binding volume %s into %s again", volumeID, target)
			err := detach(target)
			if err == nil {
				err = fusemount.Bind(st.StagingPath, target, readOnly)
			}
			if err != nil {
				klog.Errorf("binding volume %s into %s again: %v", volumeID, target, err)
			}
		case fusemount.Gone:
			delete(st.Targets, target)
			changed = true
		}
	}
	// Kubelet removed them without unstaging or unpublishing, or the plugin
	// crashed between mounting and recording the first user
	for user := range st.Users {
		if fusemount.Status(user) == fusemount.Gone {
			delete(st.Users, user)
			changed = true
		}
	}
	if len(st.Users) == 0 {
		klog.Infof("unmounting volume %s, nothing uses it", volumeID)
		if err := m.unmount(ctx, volumeID, st.StagingPath); err != nil {
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

// remount replaces the mount of st: it is unmounted, what is left of the old scope is
// stopped so that the new rclone can take its name, and rclone is started again.
func (m *mounts) remount(ctx context.Context, st volumeState) error {
	if err := detach(st.StagingPath); err != nil {
		return err
	}
	if err := m.scopes.Stop(ctx, st.VolumeID); err != nil {
		return err
	}
	return m.startRclone(ctx, st)
}

// detach unmounts path lazily. Without the privilege to unmount, as in the sanity tests,
// the kernel answers EPERM for a path that is not mounted as well, which is no failure.
func detach(path string) error {
	err := fusemount.Unmount(path)
	if errors.Is(err, syscall.EPERM) {
		if s := fusemount.Status(path); s != fusemount.Live && s != fusemount.Dead {
			return nil
		}
	}
	return err
}
