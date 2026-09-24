package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Ruakij/csi-rclone/pkg/rclone"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

var (
	endpoint       string
	nodeID         string
	scopeMemoryMax string
)

func init() {
	klog.InitFlags(nil)
}

func main() {

	_ = flag.CommandLine.Parse([]string{})

	cmd := &cobra.Command{
		Use:   "rclone",
		Short: "CSI based rclone driver",
		Run: func(_ *cobra.Command, _ []string) {
			handle()
		},
	}

	cmd.Flags().AddGoFlagSet(flag.CommandLine)

	cmd.Flags().StringVar(&nodeID, "nodeid", "", "node id")
	cobra.CheckErr(cmd.MarkFlagRequired("nodeid"))

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "CSI endpoint")
	cobra.CheckErr(cmd.MarkFlagRequired("endpoint"))

	cmd.Flags().StringVar(&rclone.DaemonLifetime, "daemon-lifetime", "auto",
		"auto, systemd or in-container: whether rclone runs in a host systemd scope and outlives the plugin")
	cmd.Flags().StringVar(&scopeMemoryMax, "scope-memory-max", "", "MemoryMax of each rclone systemd scope, e.g. 2Gi")
	cmd.Flags().Uint64Var(&rclone.ScopeTasksMax, "scope-tasks-max", 0, "TasksMax of each rclone systemd scope")
	cmd.Flags().BoolVar(&rclone.ReuseMounts, "reuse-mounts", true,
		"reuse a running rclone mount for volumes whose rclone arguments match it exactly, credentials included")
	cmd.Flags().BoolVar(&rclone.UnrestrictedOptions, "unrestricted-rclone-options", false,
		"pass every rclone option and backend through, which gives whoever writes PersistentVolumes or rclone-secret root on the node")
	cmd.Flags().StringSliceVar(&rclone.AllowedBackends, "allowed-backends", nil, "backends volumes may use, default all supported ones")
	cmd.Flags().StringSliceVar(&rclone.AllowedEndpoints, "allowed-endpoints", nil,
		"hosts that endpoint options may point to, entries starting with . match subdomains, default any")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Prints information about this version of csi rclone plugin",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf(`csi-rclone plugin
Version:    %s
`, rclone.DriverVersion)
		},
	}

	cmd.AddCommand(versionCmd)
	versionCmd.ResetFlags()

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s", err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}

func handle() {
	switch rclone.DaemonLifetime {
	case "auto", "systemd", "in-container":
	default:
		klog.Fatalf("invalid --daemon-lifetime %q", rclone.DaemonLifetime)
	}
	if scopeMemoryMax != "" {
		q, err := resource.ParseQuantity(scopeMemoryMax)
		if err != nil || q.Sign() <= 0 {
			klog.Fatalf("invalid --scope-memory-max %q", scopeMemoryMax)
		}
		rclone.ScopeMemoryMax = uint64(q.Value())
	}
	d := rclone.NewDriver(nodeID, endpoint)
	d.Run()
}
