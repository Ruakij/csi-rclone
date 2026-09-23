package rclone

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume/util"

	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type nodeServer struct {
	Driver *Driver
	*csicommon.DefaultNodeServer
	mounter *mount.SafeFormatAndMount
}

// Holds the rc socket and config file of each mount, only accessible to root.
const runtimeDir = "/tmp/csi-rclone"

// runtimePath hashes the target path to stay within the unix socket path length limit.
func runtimePath(targetPath string, ext string) string {
	sum := sha256.Sum256([]byte(targetPath))
	return filepath.Join(runtimeDir, hex.EncodeToString(sum[:])+ext)
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	glog.V(4).Infof("NodePublishVolume: volume %s, target %s, readonly %v", req.GetVolumeId(), req.GetTargetPath(), req.GetReadonly())

	targetPath := req.GetTargetPath()

	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		if _, err := ioutil.ReadDir(targetPath); err == nil {
			glog.V(4).Infof("already mounted to target %s", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}
		// todo: mount link is invalid, now unmount and remount later (built-in functionality)
		glog.Warningf("ReadDir %s failed with %v, unmount this directory", targetPath, err)

		ns.mounter = &mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      mount.NewOsExec(),
		}

		if err := ns.mounter.Unmount(targetPath); err != nil {
			glog.Errorf("Unmount directory %s failed with %v", targetPath, err)
			return nil, err
		}
	}

	// Load default connection settings from secret
	secret, _ := getSecret("rclone-secret")

	remote, remotePath, configData, flags, e := extractFlags(req.GetVolumeContext(), secret)
	if e != nil {
		glog.Warningf("storage parameter error: %s", e)
		return nil, e
	}

	e = Mount(remote, remotePath, targetPath, configData, flags, isReadOnly(req))
	if e != nil {
		if os.IsPermission(e) {
			return nil, status.Error(codes.PermissionDenied, e.Error())
		}
		if strings.Contains(e.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, e.Error())
		}
		return nil, status.Error(codes.Internal, e.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func isReadOnly(req *csi.NodePublishVolumeRequest) bool {
	readOnly := req.GetReadonly()
	switch req.GetVolumeCapability().GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY, csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		readOnly = true
	}
	for _, flag := range req.GetVolumeCapability().GetMount().GetMountFlags() {
		if flag == "ro" {
			readOnly = true
		} else {
			glog.Warningf("ignoring unsupported mount option %q, use volumeAttributes for rclone flags", flag)
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
		glog.V(4).Infof("No csi-rclone connection defaults secret found.")
	}

	if len(volumeContext) > 0 {
		for k, v := range volumeContext {
			flags[k] = v
		}
	}

	if e := validateFlags(flags); e != nil {
		return "", "", "", flags, e
	}

	remote := flags["remote"]
	remotePath := flags["remotePath"]

	if remotePathSuffix, ok := flags["remotePathSuffix"]; ok {
		remotePath = remotePath + remotePathSuffix
		delete(flags, "remotePathSuffix")
	}

	configData := ""
	ok := false

	if configData, ok = flags["configData"]; ok {
		delete(flags, "configData")
	}

	delete(flags, "remote")
	delete(flags, "remotePath")

	return remote, remotePath, configData, flags, nil
}

// https://rclone.org/rc/#core-stats
type rcCoreStatsResponse struct {
	// an array of currently active file transfers
	Transferring map[string]interface{} `json:"transferring"`
}

// https://rclone.org/rc/#vfs-stats
type rcVfsStatsResponse struct {
	DiskCache struct {
		UploadsInProgress int64 `json:"uploadsInProgress"`
		UploadsQueued     int64 `json:"uploadsQueued"`
	} `json:"diskCache"`
}

// RcloneRPC is a helper function to call rclone rc server
func RcloneRPC(socket string, method string, input string) (output string, err error) {
	url := "http://rclone/" + method

	// Create a POST request to API
	req, err := http.NewRequest("POST", url, strings.NewReader(input))
	if err != nil {
		return "", fmt.Errorf("cannot create HTTP request: %v", err)
	}

	// Set the content type to JSON
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}

	// Send the request via the client
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot send HTTP request: %v", err)
	}

	// Close the response body on function exit
	defer resp.Body.Close()

	// Read the response body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("cannot read HTTP response: %v", err)
	}

	// Return the response body as a string
	return string(body), nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	rcSocket := runtimePath(targetPath, ".sock")

	if _, err := os.Stat(rcSocket); err == nil {
		// Connect to rclone rpc server and query the operation status
		// If the rclone process is still running, wait for it to finish cache sync
		// If the rclone process is not running, proceed to volume unmount

		// check the state of the rclone process until it finishes the cache sync
		// Hard timeout is 1 hour
		copyTimeout := time.Now().Add(1 * time.Hour)
		for copyTimeout.After(time.Now()) {

			out, err := RcloneRPC(rcSocket, "core/stats", "{}")
			if err == nil {
				var coreStats rcCoreStatsResponse
				err = json.Unmarshal([]byte(out), &coreStats)
				if err == nil {
					if len(coreStats.Transferring) > 0 {
						time.Sleep(5 * time.Second)
						continue
					}
				}

			}

			out, err = RcloneRPC(rcSocket, "vfs/stats", "{}")
			if err == nil {
				var vfsStats rcVfsStatsResponse
				err = json.Unmarshal([]byte(out), &vfsStats)
				if err == nil {
					if vfsStats.DiskCache.UploadsInProgress > 0 || vfsStats.DiskCache.UploadsQueued > 0 {
						time.Sleep(5 * time.Second)
						continue
					}
				}
			}

			// proceed to volume unmount
			break
		}

		// Remove VFS cache
		os.RemoveAll("/tmp/rclone-vfs-cache/" + targetPath)
	}

	m := mount.New("")

	notMnt, err := m.IsLikelyNotMountPoint(targetPath)
	if err != nil && !mount.IsCorruptedMnt(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if notMnt && !mount.IsCorruptedMnt(err) {
		glog.V(4).Infof("Volume not mounted")

	} else {
		err = util.UnmountPath(req.GetTargetPath(), m)
		if err != nil {
			glog.V(4).Infof("Error while unmounting path: %s", err)
			// This will exit and fail the NodeUnpublishVolume making it to retry unmount on the next api schedule trigger.
			// Since we mount the volume with allow-non-empty now, we could skip this one too.
			return nil, status.Error(codes.Internal, err.Error())
		}

		glog.V(4).Infof("Volume %s unmounted successfully", req.VolumeId)
	}

	os.Remove(rcSocket)
	os.Remove(runtimePath(targetPath, ".conf"))

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return &csi.NodeStageVolumeResponse{}, nil
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

func getSecret(secretName string) (*v1.Secret, error) {
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

	glog.V(4).Infof("Loading csi-rclone connection defaults from secret %s/%s", namespace, secretName)

	secret, e := clientset.CoreV1().
		Secrets(namespace).
		Get(secretName, metav1.GetOptions{})

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

// Mount routine.
func Mount(remote string, remotePath string, targetPath string, configData string, flags map[string]string, readOnly bool) error {
	mountCmd := "rclone"
	mountArgs := []string{}

	defaultFlags := map[string]string{}
	defaultFlags["cache-info-age"] = "72h"
	defaultFlags["cache-chunk-clean-interval"] = "15m"
	defaultFlags["dir-cache-time"] = "5s"
	defaultFlags["vfs-cache-mode"] = "writes"
	defaultFlags["cache-dir"] = "/tmp/rclone-vfs-cache/" + targetPath
	defaultFlags["allow-non-empty"] = "true"
	defaultFlags["allow-other"] = "true"

	remoteWithPath := fmt.Sprintf(":%s:%s", remote, remotePath)

	if strings.Contains(configData, "["+remote+"]") {
		remoteWithPath = fmt.Sprintf("%s:%s", remote, remotePath)
		glog.V(4).Infof("remote %s found in configData, remoteWithPath set to %s", remote, remoteWithPath)
	}

	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return err
	}
	rcSocket := runtimePath(targetPath, ".sock")
	// Left over if a previous rclone on this target died
	os.Remove(rcSocket)

	// rclone mount remote:path /path/to/mountpoint [flags]
	mountArgs = append(
		mountArgs,
		"mount",
		remoteWithPath,
		targetPath,
		"--rc",
		"--rc-addr=unix://"+rcSocket,
		"--daemon",
		"--daemon-wait=0",
	)

	// Command line flags take precedence over the RCLONE_* environment set from flags
	if readOnly {
		mountArgs = append(mountArgs, "--read-only")
	}

	// rclone --daemon forks and reads the config again, and writes refreshed tokens back,
	// so the file stays until NodeUnpublishVolume
	if configData != "" {
		configFile := runtimePath(targetPath, ".conf")
		if err := ioutil.WriteFile(configFile, []byte(configData), 0600); err != nil {
			return err
		}
		mountArgs = append(mountArgs, "--config", configFile)
	} else {
		// Disable "config not found" notice
		mountArgs = append(mountArgs, "--config=")
	}

	env := os.Environ()

	// Add default flags
	for k, v := range defaultFlags {
		// Exclude overriden flags
		if _, ok := flags[k]; !ok {
			env = append(env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
		}
	}

	// Add user supplied flags
	for k, v := range flags {
		env = append(env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
	}

	// create target, os.Mkdirall is noop if it exists
	if err := os.MkdirAll(targetPath, 0750); err != nil {
		return err
	}

	glog.V(4).Infof("executing mount command cmd=%s, remote=%s, targetpath=%s", mountCmd, remoteWithPath, targetPath)
	glog.V(4).Infof("mountArgs: %v", mountArgs)

	cmd := exec.Command(mountCmd, mountArgs...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mounting failed: %v cmd: '%s' remote: '%s' targetpath: %s output: %q",
			err, mountCmd, remoteWithPath, targetPath, string(out))
	}

	return nil
}
