package rclone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

// ReuseMounts lets a volume reuse the running rclone mount of another volume with the same rclone arguments.
var ReuseMounts = true

const ephemeralContextKey = "csi.storage.k8s.io/ephemeral"

type nodeServer struct {
	csi.UnimplementedNodeServer
	Driver *Driver
}

func (ns *nodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: ns.Driver.nodeID}, nil
}

func (ns *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME},
			},
		}},
	}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID, stagingPath := req.GetVolumeId(), req.GetStagingTargetPath()
	klog.V(4).Infof("NodeStageVolume: volume %s, staging path %s", volumeID, stagingPath)
	if volumeID == "" || stagingPath == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume ID, staging target path and volume capability are required")
	}
	// Staged already, reconcile repairs a dead mount
	if st, ok := lockMountOf(stagingPath); ok {
		unlockVolume(st.VolumeID)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Load default connection settings from secret
	secret, _ := getSecret(ctx, "rclone-secret")

	a := rcloneArgs{readOnly: isReadOnly(req.GetVolumeCapability())}
	var e error
	if a.remote, a.remotePath, a.configData, a.flags, e = extractFlags(req.GetVolumeContext(), secret); e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return nil, e
	}
	// Reconcile takes a missing staging path for an unstaged volume
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	id := mountID(volumeID, a)
	lockVolume(id)
	defer unlockVolume(id)
	if _, err := acquire(ctx, id, a, stagingPath); err != nil {
		return nil, err
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID, stagingPath := req.GetVolumeId(), req.GetStagingTargetPath()
	klog.V(4).Infof("NodeUnstageVolume: volume %s, staging path %s", volumeID, stagingPath)
	if volumeID == "" || stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging target path are required")
	}

	st, ok := lockMountOf(stagingPath)
	if !ok {
		return &csi.NodeUnstageVolumeResponse{}, nil
	}
	defer unlockVolume(st.VolumeID)
	if err := release(ctx, st, stagingPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

type rcloneArgs struct {
	remote, remotePath, configData string
	flags                          map[string]string
	readOnly                       bool
}

// mountID names the rclone mount of a volume. With ReuseMounts, volumes with
// the same arguments, credentials included, get the same mount.
func mountID(volumeID string, a rcloneArgs) string {
	if !ReuseMounts {
		return volumeID
	}
	// Cannot fail for these types, and sorts map keys, so equal arguments give equal IDs
	data, _ := json.Marshal([]any{a.remote, a.remotePath, a.configData, a.flags, a.readOnly})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// acquire adds user to the rclone mount id, mounting it unless it is live. The caller holds the lock on id.
func acquire(ctx context.Context, id string, a rcloneArgs, user string) (volumeState, error) {
	st, err := loadState(id)
	switch {
	case os.IsNotExist(err):
		if err := mountRclone(ctx, id, a); err != nil {
			return st, err
		}
		if st, err = loadState(id); err != nil {
			return st, status.Error(codes.Internal, err.Error())
		}
	case err != nil:
		return st, status.Error(codes.Internal, err.Error())
	case !isFUSEMount(st.StagingPath):
		if err := remount(ctx, st); err != nil {
			return st, status.Error(codes.Internal, err.Error())
		}
		st.Mounted = true
	}
	if st.Users == nil {
		st.Users = map[string]struct{}{}
	}
	st.Users[user] = struct{}{}
	if err := saveState(st); err != nil {
		return st, status.Error(codes.Internal, err.Error())
	}
	return st, nil
}

func mountRclone(ctx context.Context, id string, a rcloneArgs) error {
	// A dead FUSE mount from an rclone that exited
	if err := lazyUnmount(mountPath(id)); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	e := Mount(ctx, id, a.remote, a.remotePath, mountPath(id), a.configData, a.flags, a.readOnly)
	if e == nil {
		return nil
	}
	if os.IsPermission(e) {
		return status.Error(codes.PermissionDenied, e.Error())
	}
	if strings.Contains(e.Error(), "invalid argument") {
		return status.Error(codes.InvalidArgument, e.Error())
	}
	return status.Error(codes.Internal, e.Error())
}

// release removes user from the mount, and unmounts it once it has none. The caller holds the lock.
func release(ctx context.Context, st volumeState, user string) error {
	delete(st.Users, user)
	if len(st.Users) > 0 {
		return saveState(st)
	}
	return unmount(ctx, st.VolumeID, st.StagingPath)
}

// unmount drains uploads, unmounts rclone and removes its state. The caller holds the lock.
func unmount(ctx context.Context, id, mountPoint string) error {
	waitForUploads(id)
	if err := lazyUnmount(mountPoint); err != nil {
		return err
	}
	stopScope(ctx, id)
	// Fails if still mounted, so RemoveAll cannot delete files on the remote
	if mountPoint == mountPath(id) {
		if err := os.Remove(mountPoint); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.RemoveAll(volumeDir(id))
}

// ephemeralArgs builds the rclone arguments of an ephemeral volume. Pod creators define
// these, so rclone-secret is not applied: its credentials would let them mount any path.
// Credentials come from configData, volumeAttributes or nodePublishSecretRef.
func ephemeralArgs(req *csi.NodePublishVolumeRequest) (rcloneArgs, error) {
	var secret *v1.Secret
	if len(req.GetSecrets()) > 0 {
		secret = &v1.Secret{Data: map[string][]byte{}}
		for k, v := range req.GetSecrets() {
			secret.Data[k] = []byte(v)
		}
	}
	a := rcloneArgs{readOnly: isReadOnly(req.GetVolumeCapability())}
	var err error
	a.remote, a.remotePath, a.configData, a.flags, err = extractFlags(req.GetVolumeContext(), secret)
	return a, err
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID, stagingPath, targetPath := req.GetVolumeId(), req.GetStagingTargetPath(), req.GetTargetPath()
	klog.V(4).Infof("NodePublishVolume: volume %s, target %s, readonly %v", volumeID, targetPath, req.GetReadonly())
	if volumeID == "" || targetPath == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume ID, target path and volume capability are required")
	}

	var st volumeState
	if stagingPath == "" {
		// Kubelet does not stage ephemeral volumes, so the target is the mount's user
		if req.GetVolumeContext()[ephemeralContextKey] != "true" {
			return nil, status.Error(codes.InvalidArgument, "staging target path is required")
		}
		a, err := ephemeralArgs(req)
		if err != nil {
			klog.Warningf("storage parameter error: %s", err)
			return nil, err
		}
		id := mountID(volumeID, a)
		lockVolume(id)
		defer unlockVolume(id)
		if st, err = acquire(ctx, id, a, targetPath); err != nil {
			return nil, err
		}
	} else {
		var ok bool
		if st, ok = lockMountOf(stagingPath); !ok {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is not staged at %s", volumeID, stagingPath)
		}
		defer unlockVolume(st.VolumeID)
	}

	if !isFUSEMount(st.StagingPath) {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is not mounted at %s", volumeID, st.StagingPath)
	}
	// Recorded before binding, so reconcile can bind it again after rclone dies
	readOnly := req.GetReadonly() || isReadOnly(req.GetVolumeCapability())
	if st.Targets == nil {
		st.Targets = map[string]bool{}
	}
	st.Targets[targetPath] = readOnly
	if err := saveState(st); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if isFUSEMount(targetPath) {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if err := lazyUnmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if err := os.MkdirAll(targetPath, 0750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := bindMount(st.StagingPath, targetPath, readOnly); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID, targetPath := req.GetVolumeId(), req.GetTargetPath()
	klog.V(4).Infof("NodeUnpublishVolume: volume %s, target %s", volumeID, targetPath)
	if volumeID == "" || targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}

	st, ok := lockMountOf(targetPath)
	if ok {
		defer unlockVolume(st.VolumeID)
	}
	if err := lazyUnmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	delete(st.Targets, targetPath)
	var err error
	if _, user := st.Users[targetPath]; user {
		err = release(ctx, st, targetPath)
	} else {
		err = saveState(st)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// isReadOnly reports whether the volume is read-only for every pod, so rclone itself mounts it read-only.
func isReadOnly(capability *csi.VolumeCapability) bool {
	readOnly := false
	switch capability.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY, csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		readOnly = true
	}
	for _, flag := range capability.GetMount().GetMountFlags() {
		if flag == "ro" {
			readOnly = true
		} else {
			klog.Warningf("ignoring unsupported mount option %q, use volumeAttributes for rclone flags", flag)
		}
	}
	return readOnly
}

func extractFlags(volumeContext map[string]string, secret *v1.Secret) (string, string, string, map[string]string, error) {

	// Empty argument list
	flags := make(map[string]string)

	// Secret values are default, gets merged and overriden by corresponding PV values
	if secret != nil && secret.Data != nil && len(secret.Data) > 0 {

		// Needs byte to string casting for map values
		for k, v := range secret.Data {
			flags[k] = string(v)
		}
	} else {
		klog.V(4).Infof("No csi-rclone connection defaults secret found.")
	}

	for k, v := range volumeContext {
		// Keys Kubernetes adds, like storage.kubernetes.io/csiProvisionerIdentity
		if !strings.Contains(k, "/") {
			flags[k] = v
		}
	}

	if e := validateFlags(flags); e != nil {
		return "", "", "", flags, e
	}

	remote := flags["remote"]
	remotePath := flags["remotePath"]

	if remotePathSuffix, ok := flags["remotePathSuffix"]; ok {
		remotePath += remotePathSuffix
		delete(flags, "remotePathSuffix")
	}

	configData, ok := flags["configData"]
	if ok {
		delete(flags, "configData")
	}

	delete(flags, "remote")
	delete(flags, "remotePath")

	if e := validateOptions(remote, configData, flags); e != nil {
		return "", "", "", flags, e
	}

	return remote, remotePath, configData, flags, nil
}

func validateFlags(flags map[string]string) error {
	if _, ok := flags["remote"]; !ok {
		return status.Errorf(codes.InvalidArgument, "missing volume context value: remote")
	}
	if _, ok := flags["remotePath"]; !ok {
		return status.Errorf(codes.InvalidArgument, "missing volume context value: remotePath")
	}
	return nil
}

func getSecret(ctx context.Context, secretName string) (*v1.Secret, error) {
	clientset, e := GetK8sClient()
	if e != nil {
		return nil, status.Errorf(codes.Internal, "can not create kubernetes client: %s", e)
	}

	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	namespace, _, err := kubeconfig.Namespace()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "can't get current namespace for secret %s: %s", secretName, err)
	}

	klog.V(4).Infof("Loading csi-rclone connection defaults from secret %s/%s", namespace, secretName)

	secret, e := clientset.CoreV1().
		Secrets(namespace).
		Get(ctx, secretName, metav1.GetOptions{})

	if e != nil {
		return nil, status.Errorf(codes.Internal, "can't load csi-rclone settings from secret %s: %s", secretName, e)
	}

	return secret, nil
}

func flagToEnvName(flag string) string {
	// To find the name of the environment variable, first, take the long option name, strip the leading --, change - to _, make upper case and prepend RCLONE_.
	flag = strings.TrimPrefix(flag, "--") // we dont pass prefixed args, but strictly this is the algorithm
	flag = strings.ReplaceAll(flag, "-", "_")
	flag = strings.ToUpper(flag)
	return fmt.Sprintf("RCLONE_%s", flag)
}
