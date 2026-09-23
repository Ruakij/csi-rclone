package rclone

import (
	"context"
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

	lockVolume(volumeID)
	defer unlockVolume(volumeID)

	if isFUSEMount(stagingPath) {
		return &csi.NodeStageVolumeResponse{}, nil
	}
	// A dead FUSE mount from an rclone that exited
	if err := lazyUnmount(stagingPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Load default connection settings from secret
	secret, _ := getSecret(ctx, "rclone-secret")

	remote, remotePath, configData, flags, e := extractFlags(req.GetVolumeContext(), secret)
	if e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return nil, e
	}

	e = Mount(ctx, volumeID, remote, remotePath, stagingPath, configData, flags, isReadOnly(req.GetVolumeCapability()))
	if e != nil {
		if os.IsPermission(e) {
			return nil, status.Error(codes.PermissionDenied, e.Error())
		}
		if strings.Contains(e.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, e.Error())
		}
		return nil, status.Error(codes.Internal, e.Error())
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID, stagingPath := req.GetVolumeId(), req.GetStagingTargetPath()
	klog.V(4).Infof("NodeUnstageVolume: volume %s, staging path %s", volumeID, stagingPath)
	if volumeID == "" || stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging target path are required")
	}

	lockVolume(volumeID)
	defer unlockVolume(volumeID)

	waitForUploads(volumeID)
	if err := lazyUnmount(stagingPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	stopScope(ctx, volumeID)
	if err := os.RemoveAll(volumeDir(volumeID)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID, stagingPath, targetPath := req.GetVolumeId(), req.GetStagingTargetPath(), req.GetTargetPath()
	klog.V(4).Infof("NodePublishVolume: volume %s, target %s, readonly %v", volumeID, targetPath, req.GetReadonly())
	if volumeID == "" || stagingPath == "" || targetPath == "" || req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume ID, staging target path, target path and volume capability are required")
	}

	lockVolume(volumeID)
	defer unlockVolume(volumeID)

	if !isFUSEMount(stagingPath) {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is not mounted at %s", volumeID, stagingPath)
	}
	// Recorded before binding, so reconcile can bind it again after rclone dies
	readOnly := req.GetReadonly() || isReadOnly(req.GetVolumeCapability())
	if err := updateTargets(volumeID, targetPath, &readOnly); err != nil {
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
	if err := bindMount(stagingPath, targetPath, readOnly); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID, targetPath := req.GetVolumeId(), req.GetTargetPath()
	klog.V(4).Infof("NodeUnpublishVolume: volume %s, target %s", volumeID, targetPath)
	if volumeID == "" || targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and target path are required")
	}

	lockVolume(volumeID)
	defer unlockVolume(volumeID)

	if err := lazyUnmount(targetPath); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := updateTargets(volumeID, targetPath, nil); err != nil && !os.IsNotExist(err) {
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
