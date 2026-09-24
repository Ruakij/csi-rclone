package rclone

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

type controllerServer struct {
	csi.UnimplementedControllerServer
}

type pvcMetadata struct {
	data        map[string]string
	labels      map[string]string
	annotations map[string]string
}

// source: https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner/blob/master/cmd/nfs-subdir-external-provisioner/provisioner.go
var pattern = regexp.MustCompile(`\${\.PVC\.((labels|annotations)\.(.*?)|.*?)}`)

// source: https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner/blob/master/cmd/nfs-subdir-external-provisioner/provisioner.go
func (meta *pvcMetadata) stringParser(str string) string {
	result := pattern.FindAllStringSubmatch(str, -1)
	for _, r := range result {
		switch r[2] {
		case "labels":
			str = strings.ReplaceAll(str, r[0], meta.labels[r[3]])
		case "annotations":
			str = strings.ReplaceAll(str, r[0], meta.annotations[r[3]])
		default:
			str = strings.ReplaceAll(str, r[0], meta.data[r[1]])
		}
	}

	return str
}

const namespacePlaceholder = "${.PVC.namespace}"

// Anything tenant-controlled before the namespace segment would let PVCs in different namespaces
// resolve to the same or nested remote paths.
func validatePathPatternIsolation(pathPattern string) error {
	i := strings.Index(pathPattern, namespacePlaceholder)
	prefix, rest := "", ""
	if i >= 0 {
		prefix, rest = pathPattern[:i], pathPattern[i+len(namespacePlaceholder):]
	}
	if i < 0 || strings.Contains(prefix, "${") || (prefix != "" && !strings.HasSuffix(prefix, "/")) || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return status.Errorf(codes.InvalidArgument,
			`pathPattern %q must contain %s as a whole path segment with only fixed text before it; set sharedRemote: "true" to share the remote path between namespaces`,
			pathPattern, namespacePlaceholder)
	}
	return nil
}

// Labels and annotations are set by PVC owners, so the expanded path must not leave the pattern's prefix.
func validateRemotePathSuffix(suffix string) error {
	for _, segment := range strings.Split(suffix, "/") {
		if segment == "." || segment == ".." {
			return status.Errorf(codes.InvalidArgument, "pathPattern expands to %q, which contains a %q segment", suffix, segment)
		}
	}
	return nil
}

func (cs *controllerServer) getPVC(ctx context.Context, name, namespace string) (*v1.PersistentVolumeClaim, error) {
	clientset, e := GetK8sClient()
	if e != nil {
		return nil, status.Errorf(codes.Internal, "can not create kubernetes client: %s", e)
	}

	// Get the PVC
	pvc, err := clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("Failed to get PVC: %v", err)
		return nil, err
	}

	return pvc, nil
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := validateCapabilities(req.GetName(), req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}
	// Parse the request to get the volume name, size, and parameters.
	volumeName := req.GetName()
	capacityBytes := req.GetCapacityRange().GetRequiredBytes()

	// Extract parameters from the request
	parameters := req.GetParameters()

	volumeContext := map[string]string{}

	pvcName := ""
	pvcNamespace := ""

	// parameter provided by external-provisioner (csi-provisioner)
	if val, ok := parameters["csi.storage.k8s.io/pvc/name"]; ok {
		pvcName = val
	}

	// parameter provided by external-provisioner (csi-provisioner)
	if val, ok := parameters["csi.storage.k8s.io/pvc/namespace"]; ok {
		pvcNamespace = val
	}

	if parameters["sharedRemote"] != "true" {
		if err := validatePathPatternIsolation(parameters["pathPattern"]); err != nil {
			return nil, err
		}
		if pvcName == "" || pvcNamespace == "" {
			return nil, status.Error(codes.InvalidArgument, "PVC name and namespace missing, run csi-provisioner with --extra-create-metadata")
		}
	}

	// If PVC name is provided, load the PVC definition
	if pvcName != "" {

		pvc, err := cs.getPVC(ctx, pvcName, pvcNamespace)
		if err != nil {
			klog.Errorf("Failed to get PVC %s in namespace %s: %v", pvcName, pvcNamespace, err)
			return nil, err
		}

		// Extract PVC metadata
		metadata := &pvcMetadata{
			data: map[string]string{
				"name":      pvcName,
				"namespace": pvcNamespace,
			},
			labels:      pvc.Labels,
			annotations: pvc.Annotations,
		}

		if pathPattern, ok := parameters["pathPattern"]; ok {
			if pathPattern != "" {
				remotePathSuffix := metadata.stringParser(pathPattern)
				if remotePathSuffix != "" {
					if err := validateRemotePathSuffix(remotePathSuffix); err != nil {
						return nil, err
					}
					if !strings.HasPrefix(remotePathSuffix, "/") {
						remotePathSuffix = "/" + remotePathSuffix
					}
					volumeContext["remotePathSuffix"] = remotePathSuffix
				}
			}
		}

		// if Annotation starts with "csi-rclone/", extract the key and value from the annotation
		for key, value := range metadata.annotations {
			if strings.HasPrefix(key, "csi-rclone/") {
				key = strings.TrimPrefix(key, "csi-rclone/")

				// Only allow some keys (umask, uid) to be passed to the volume context to avoid security issues
				if key == "umask" {
					if _, err := strconv.ParseUint(value, 8, 32); err != nil {
						return nil, status.Errorf(codes.InvalidArgument, "annotation csi-rclone/umask %q is not an octal number", value)
					}
					volumeContext[key] = value
				}
			}
		}
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeName,
			CapacityBytes: capacityBytes,
			VolumeContext: volumeContext,
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	caps := req.GetVolumeCapabilities()
	if err := validateCapabilities(req.GetVolumeId(), caps); err != nil {
		return nil, err
	}
	for _, c := range caps {
		if c.GetMount() == nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "only filesystem volumes are supported"}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: caps,
			Parameters:         req.GetParameters(),
		},
	}, nil
}

func validateCapabilities(id string, caps []*csi.VolumeCapability) error {
	if id == "" || len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume name or ID and volume capabilities are required")
	}
	return nil
}

func (cs *controllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME},
			},
		}},
	}, nil
}
