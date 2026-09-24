//go:build linux

package rclone

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func TestSanity(t *testing.T) {
	// Short, since unix socket paths are capped near 100 bytes.
	dir, err := os.MkdirTemp("", "csi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	StateDir = filepath.Join(dir, "state")
	sock := filepath.Join(dir, "csi.sock")

	d := NewDriver("node", "unix://"+sock)
	go serve(d.endpoint, d, d.cs, d.ns)

	cfg := sanity.NewTestConfig()
	cfg.Address = sock
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	cfg.TestVolumeParameters = map[string]string{"sharedRemote": "true"}
	sc := sanity.GinkgoTest(&cfg)
	gomega.RegisterFailHandler(ginkgo.Fail)

	suite, reporter := ginkgo.GinkgoConfiguration()
	// Volumes are stateless names, and publishing needs a remote to mount, which e2e covers.
	suite.SkipStrings = append(suite.SkipStrings, "existing name and different capacity",
		"requested volume does not exist", "should remove target path", `Node Service should (work|be idempotent)`)
	ginkgo.RunSpecs(t, "CSI sanity", suite, reporter)
	sc.Finalize()
}
