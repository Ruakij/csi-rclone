package rclone

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog/v2"
)

type Driver struct {
	csi.UnimplementedIdentityServer

	nodeID   string
	endpoint string

	ns *nodeServer
	cs *controllerServer
}

var (
	DriverName    = "csi-rclone"
	DriverVersion = "latest"
)

func NewDriver(nodeID, endpoint string) *Driver {
	klog.Infof("Starting new %s driver in version %s", DriverName, DriverVersion)

	d := &Driver{nodeID: nodeID, endpoint: endpoint}
	d.cs = &controllerServer{}
	d.ns = &nodeServer{Driver: d}
	return d
}

func (d *Driver) Run() {
	go reconcile(context.Background())
	serve(d.endpoint, d, d.cs, d.ns)
}

func (d *Driver) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: DriverVersion}, nil
}

func (d *Driver) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{{
			Type: &csi.PluginCapability_Service_{
				Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE},
			},
		}},
	}, nil
}

func (d *Driver) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}
