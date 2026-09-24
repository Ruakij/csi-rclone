//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	driverNS = "csi-rclone"
	ns       = "e2e"
	image    = "csi-rclone"
	busybox  = "busybox:1.37"
	rclone   = "rclone/rclone:1.75.1"
	release  = "csi-rclone"
	repoRoot = "../.."
)

var (
	cluster = envOr("E2E_CLUSTER", "csi-rclone-e2e")
	kubeCtx = "kind-" + cluster
	client  kubernetes.Interface
	node    string
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	code := 1
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
	} else {
		code = m.Run()
	}
	if code != 0 {
		diagnose()
	}
	if os.Getenv("E2E_KEEP") == "" {
		_ = stream(exec.Command("kind", "delete", "cluster", "--name", cluster))
	}
	os.Exit(code)
}

func setup() error {
	if out, _ := run("kind", "get", "clusters"); !slices.Contains(strings.Fields(out), cluster) {
		if err := stream(exec.Command("kind", "create", "cluster", "--name", cluster, "--wait", "120s")); err != nil {
			return err
		}
	}
	if os.Getenv("E2E_SKIP_BUILD") == "" {
		if err := stream(exec.Command("docker", "build", "-t", image+":e2e", repoRoot)); err != nil {
			return err
		}
	}
	// Tagged by content, so a rebuilt image changes the pod template and rolls the driver.
	id, err := run("docker", "image", "inspect", "-f", "{{.Id}}", image+":e2e")
	if err != nil {
		return err
	}
	tag := "e2e-" + strings.TrimPrefix(id, "sha256:")[:12]
	if _, err := run("docker", "tag", image+":e2e", image+":"+tag); err != nil {
		return err
	}
	if err := stream(exec.Command("kind", "load", "docker-image", "--name", cluster, image+":"+tag)); err != nil {
		return err
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeCtx}).ClientConfig()
	if err != nil {
		return err
	}
	if client, err = kubernetes.NewForConfig(cfg); err != nil {
		return err
	}
	ctx := context.Background()
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	node = nodes.Items[0].Name

	for _, n := range []string{driverNS, ns} {
		_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	if err := s3Server(ctx); err != nil {
		return err
	}

	endpoint := "s3." + driverNS + ".svc.cluster.local"
	return stream(exec.Command("helm", "--kube-context", kubeCtx, "upgrade", "--install", release, repoRoot+"/charts/csi-rclone",
		"-n", driverNS, "--wait", "--timeout", "3m",
		"--set", "image.repository="+image, "--set", "image.tag="+tag, "--set", "image.pullPolicy=Never",
		"--set", "storageClass.create=true",
		"--set", "allowedEndpoints={"+endpoint+"}",
		"--set", "rcloneSecret.create=true",
		"--set", "rcloneSecret.stringData.remote=s3",
		"--set", "rcloneSecret.stringData.remotePath=bucket",
		"--set", "rcloneSecret.stringData.s3-provider=Rclone",
		"--set", "rcloneSecret.stringData.s3-endpoint=http://"+endpoint+":9000",
		"--set", "rcloneSecret.stringData.s3-access-key-id=key",
		"--set", "rcloneSecret.stringData.s3-secret-access-key=secret"))
}

// s3Server is the remote all volumes use, serving the bucket from an emptyDir.
func s3Server(ctx context.Context) error {
	labels := map[string]string{"app": "s3"}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "s3", Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "s3", Image: rclone,
				Args:         []string{"serve", "s3", "/data", "--addr=:9000", "--auth-key=key,secret"},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data/bucket"}},
			}},
			Volumes: []corev1.Volume{emptyDir("data")},
		},
	}
	if _, err := client.CoreV1().Pods(driverNS).Create(ctx, p, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "s3"},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 9000, TargetPort: intstr.FromInt32(9000)}},
		},
	}
	if _, err := client.CoreV1().Services(driverNS).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		return isReady(ctx, driverNS, "s3")
	})
}

func TestVolumes(t *testing.T) {
	createPVC(t, "data", "rclone", "")
	create(t, pod("writer", claim("data", false)))
	ready(t, "writer")

	t.Run("writes reach the remote", func(t *testing.T) {
		expect(t, "writer", "echo hello > /data/f && cat /data/f", "hello")
		poll(t, time.Minute, "file on the remote", func(context.Context) (bool, error) {
			out, _ := run("kubectl", "--context", kubeCtx, "-n", driverNS, "exec", "s3", "--", "cat", "/data/bucket/"+ns+"/data/f")
			return out == "hello", nil
		})
	})

	t.Run("readOnly volume refuses writes", func(t *testing.T) {
		create(t, pod("reader", claim("data", true)))
		t.Cleanup(func() { _ = deletePods(context.Background(), "reader") })
		ready(t, "reader")
		expect(t, "reader", "cat /data/f", "hello")
		expectFail(t, "reader", "echo x > /data/x")
	})

	t.Run("options outside the allow-list are refused", func(t *testing.T) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-local"},
			Spec: corev1.PersistentVolumeSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				ClaimRef:         &corev1.ObjectReference{Namespace: ns, Name: "local"},
				StorageClassName: "",
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver:           "csi-rclone",
					VolumeHandle:     "e2e-local",
					VolumeAttributes: map[string]string{"remote": "local", "remotePath": "/etc"},
				}},
			},
		}
		if _, err := client.CoreV1().PersistentVolumes().Create(t.Context(), pv, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = client.CoreV1().PersistentVolumes().Delete(context.Background(), pv.Name, metav1.DeleteOptions{})
		})
		createPVC(t, "local", "", pv.Name)
		p := create(t, pod("local", claim("local", false)))
		t.Cleanup(func() { _ = deletePods(context.Background(), "local") })
		failedMount(t, p, "InvalidArgument")
	})

	t.Run("mounts survive a driver restart", func(t *testing.T) {
		before := driverPod(t)
		kubectl(t, "-n", driverNS, "rollout", "restart", "ds/"+release+"-node")
		kubectl(t, "-n", driverNS, "rollout", "status", "ds/"+release+"-node", "--timeout=120s")
		if driverPod(t) == before {
			t.Fatal("driver pod was not replaced")
		}
		expect(t, "writer", "cat /data/f", "hello")
		expect(t, "writer", "echo after > /data/after && cat /data/after", "after")
	})

	// SIGKILL leaves a dead FUSE mount, SIGTERM makes rclone unmount before exiting.
	for _, sig := range []string{"KILL", "TERM"} {
		t.Run("rclone is remounted after SIG"+sig, func(t *testing.T) {
			if _, err := run("docker", "exec", node, "pkill", "-"+sig, "-f", "^rclone mount"); err != nil {
				t.Fatal(err)
			}
			// Running containers keep the dead mount, a new pod gets the remounted one.
			create(t, pod("late", claim("data", false)))
			t.Cleanup(func() { _ = deletePods(context.Background(), "late") })
			ready(t, "late")
			expect(t, "late", "cat /data/f", "hello")
		})
	}

	t.Run("unstaging leaves nothing behind", func(t *testing.T) {
		if err := deletePods(t.Context(), "writer", "late"); err != nil {
			t.Fatal(err)
		}
		if err := client.CoreV1().PersistentVolumeClaims(ns).Delete(t.Context(), "data", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		poll(t, 2*time.Minute, "no rclone scopes, state or mounts on the node", func(context.Context) (bool, error) {
			out, err := run("docker", "exec", node, "sh", "-c",
				"systemctl list-units --no-legend 'csi-rclone-*'; ls -A /var/lib/csi-rclone; grep rclone /proc/self/mountinfo; true")
			return err == nil && out == "", nil
		})
	})
}

// TestSharedMounts mounts one remote as an ephemeral volume with credentials from a
// Secret and as a PersistentVolume with them in volumeAttributes: both reuse one rclone.
func TestSharedMounts(t *testing.T) {
	attrs := map[string]string{
		"remote": "s3", "remotePath": "bucket/shared", "s3-provider": "Rclone",
		"s3-endpoint": "http://s3." + driverNS + ".svc.cluster.local:9000",
	}
	creds := map[string]string{"s3-access-key-id": "key", "s3-secret-access-key": "secret"}
	pvAttrs := maps.Clone(attrs)
	maps.Copy(pvAttrs, creds)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3-credentials"}, StringData: creds}
	if _, err := client.CoreV1().Secrets(ns).Create(t.Context(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Secrets(ns).Delete(context.Background(), secret.Name, metav1.DeleteOptions{})
	})
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-shared"},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Capacity:         corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			ClaimRef:         &corev1.ObjectReference{Namespace: ns, Name: "shared"},
			StorageClassName: "",
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "csi-rclone", VolumeHandle: "e2e-shared", VolumeAttributes: pvAttrs,
			}},
		},
	}
	if _, err := client.CoreV1().PersistentVolumes().Create(t.Context(), pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().PersistentVolumes().Delete(context.Background(), pv.Name, metav1.DeleteOptions{})
	})
	createPVC(t, "shared", "", pv.Name)

	create(t, pod("ephemeral", corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver: "csi-rclone", VolumeAttributes: attrs, NodePublishSecretRef: &corev1.LocalObjectReference{Name: secret.Name},
	}}}))
	create(t, pod("persistent", claim("shared", false)))
	t.Cleanup(func() { _ = deletePods(context.Background(), "ephemeral", "persistent") })
	ready(t, "ephemeral")
	ready(t, "persistent")

	rclones := func() string {
		out, _ := run("docker", "exec", node, "sh", "-c", "pgrep -fc '^rclone mount :s3:bucket/shared'; true")
		return out
	}
	t.Run("volumes with the same arguments reuse one rclone", func(t *testing.T) {
		expect(t, "ephemeral", "echo shared > /data/f && cat /data/f", "shared")
		expect(t, "persistent", "cat /data/f", "shared")
		if n := rclones(); n != "1" {
			t.Fatalf("%s rclone processes, want 1", n)
		}
	})

	t.Run("the last volume unmounts it", func(t *testing.T) {
		if err := deletePods(t.Context(), "ephemeral"); err != nil {
			t.Fatal(err)
		}
		expect(t, "persistent", "cat /data/f", "shared")
		if err := deletePods(t.Context(), "persistent"); err != nil {
			t.Fatal(err)
		}
		poll(t, time.Minute, "no shared rclone", func(context.Context) (bool, error) {
			return rclones() == "0", nil
		})
	})
}

func pod(name string, vols ...corev1.Volume) *corev1.Pod {
	c := corev1.Container{Name: "app", Image: busybox, Command: []string{"sleep", "infinity"}}
	for _, v := range vols {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: v.Name, MountPath: "/data"})
	}
	zero := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: &zero,
			Containers:                    []corev1.Container{c},
			Volumes:                       vols,
		},
	}
}

func emptyDir(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

func claim(name string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name, ReadOnly: readOnly}}}
}

func createPVC(t *testing.T, name, storageClass, volumeName string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &storageClass,
			VolumeName:       volumeName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(ns).Create(t.Context(), pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().PersistentVolumeClaims(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

func create(t *testing.T, p *corev1.Pod) *corev1.Pod {
	t.Helper()
	p, err := client.CoreV1().Pods(ns).Create(t.Context(), p, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func deletePods(ctx context.Context, names ...string) error {
	for _, n := range names {
		err := client.CoreV1().Pods(ns).Delete(ctx, n, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		for _, n := range names {
			if _, err := client.CoreV1().Pods(ns).Get(ctx, n, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				return false, nil
			}
		}
		return true, nil
	})
}

func isReady(ctx context.Context, namespace, name string) (bool, error) {
	p, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

func ready(t *testing.T, name string) {
	t.Helper()
	poll(t, 3*time.Minute, name+" ready", func(ctx context.Context) (bool, error) {
		return isReady(ctx, ns, name)
	})
}

// failedMount keys on the pod UID: events outlive a deleted pod of the same name.
func failedMount(t *testing.T, p *corev1.Pod, substr string) {
	t.Helper()
	poll(t, 2*time.Minute, fmt.Sprintf("FailedMount event on %s containing %q", p.Name, substr), func(ctx context.Context) (bool, error) {
		evs, err := client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.uid=" + string(p.UID) + ",reason=FailedMount"})
		if err != nil {
			return false, err
		}
		for _, e := range evs.Items {
			if strings.Contains(e.Message, substr) {
				return true, nil
			}
		}
		return false, nil
	})
}

func driverPod(t *testing.T) types.UID {
	t.Helper()
	pods, err := client.CoreV1().Pods(driverNS).List(t.Context(),
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + release + ",app.kubernetes.io/component=node"})
	if err != nil {
		t.Fatal(err)
	}
	// Rollout status is done once the new pod is ready, while the old one may still be terminating.
	pods.Items = slices.DeleteFunc(pods.Items, func(p corev1.Pod) bool { return p.DeletionTimestamp != nil })
	if len(pods.Items) != 1 {
		t.Fatalf("%d running node plugin pods, want 1", len(pods.Items))
	}
	return pods.Items[0].UID
}

func poll(t *testing.T, timeout time.Duration, what string, cond wait.ConditionWithContextFunc) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), 2*time.Second, timeout, true, cond); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}

func exe(podName, cmd string) (string, error) {
	return run("kubectl", "--context", kubeCtx, "-n", ns, "exec", podName, "-c", "app", "--", "sh", "-c", cmd)
}

func expect(t *testing.T, podName, cmd, want string) {
	t.Helper()
	got, err := exe(podName, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: %q printed %q, want %q", podName, cmd, got, want)
	}
}

func expectFail(t *testing.T, podName, cmd string) {
	t.Helper()
	if _, err := exe(podName, cmd); err == nil {
		t.Fatalf("%s: %q succeeded, want failure", podName, cmd)
	}
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	if _, err := run("kubectl", append([]string{"--context", kubeCtx}, args...)...); err != nil {
		t.Fatal(err)
	}
}

// run returns trimmed stdout; the error carries stderr.
func run(name string, args ...string) (string, error) {
	var stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

// stream runs a long setup step with its output visible.
func stream(cmd *exec.Cmd) error {
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func diagnose() {
	for _, args := range [][]string{
		{"-n", ns, "get", "pods,pvc", "-o", "wide"},
		{"-n", ns, "get", "events", "--sort-by=.lastTimestamp"},
		{"-n", driverNS, "logs", "-l", "app.kubernetes.io/component=node", "-c", "rclone", "--tail=80", "--prefix"},
		{"-n", driverNS, "logs", "-l", "app.kubernetes.io/component=controller", "--all-containers", "--tail=40", "--prefix"},
	} {
		_ = stream(exec.Command("kubectl", append([]string{"--context", kubeCtx}, args...)...))
	}
}
