// Package e2e installs the chart into a kind cluster next to an in-cluster
// `rclone serve s3` and checks the volumes from real pods. The tests are behind the
// e2e build tag; run them with make e2e. E2E_KEEP=1 leaves the cluster running,
// E2E_SKIP_BUILD=1 reuses the last built csi-rclone:e2e.
package e2e
