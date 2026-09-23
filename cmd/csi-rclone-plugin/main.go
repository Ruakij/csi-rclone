package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/wunderio/csi-rclone/pkg/rclone"
	"k8s.io/klog/v2"
)

var (
	endpoint string
	nodeID   string
)

func init() {
	klog.InitFlags(nil)
}

func main() {

	flag.CommandLine.Parse([]string{})

	cmd := &cobra.Command{
		Use:   "rclone",
		Short: "CSI based rclone driver",
		Run: func(cmd *cobra.Command, args []string) {
			handle()
		},
	}

	cmd.Flags().AddGoFlagSet(flag.CommandLine)

	cmd.Flags().StringVar(&nodeID, "nodeid", "", "node id")
	cmd.MarkFlagRequired("nodeid")

	cmd.Flags().StringVar(&endpoint, "endpoint", "", "CSI endpoint")
	cmd.MarkFlagRequired("endpoint")

	cmd.Flags().BoolVar(&rclone.UnrestrictedOptions, "unrestricted-rclone-options", false,
		"pass every rclone option and backend through, which gives whoever writes PersistentVolumes or rclone-secret root on the node")
	cmd.Flags().StringSliceVar(&rclone.AllowedBackends, "allowed-backends", nil, "backends volumes may use, default all supported ones")
	cmd.Flags().StringSliceVar(&rclone.AllowedEndpoints, "allowed-endpoints", nil,
		"hosts that endpoint options may point to, entries starting with . match subdomains, default any")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Prints information about this version of csi rclone plugin",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf(`csi-rclone plugin
Version:    %s
`, rclone.DriverVersion)
		},
	}

	cmd.AddCommand(versionCmd)
	versionCmd.ResetFlags()

	cmd.ParseFlags(os.Args[1:])
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s", err.Error())
		os.Exit(1)
	}

	os.Exit(0)
}

func handle() {
	d := rclone.NewDriver(nodeID, endpoint)
	d.Run()
}
