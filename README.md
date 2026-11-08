# csi-rclone

[![Helm](https://img.shields.io/badge/dynamic/yaml?url=https%3A%2F%2Fruakij.github.io%2Fcsi-rclone%2Findex.yaml&query=%24.entries%5B%27csi-rclone%27%5D%5B0%5D.version&label=Helm&logo=helm&color=0F1689&prefix=v)](#install)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25%2B-326CE5?logo=kubernetes&logoColor=white)](#requirements)

**An rclone mount CSI driver for Kubernetes, whose mounts survive driver restarts.**

Mounts any remote [rclone](https://rclone.org/) supports (S3, GCS, Azure Blob, WebDAV,
SFTP and more) as a pod volume through
[`rclone mount`](https://rclone.org/commands/rclone_mount/), instead of a hand-managed
`rclone mount` on the node that dies with whatever started it. Volumes come from static
PersistentVolumes, from a StorageClass that gives each PVC its own path on one remote,
or inline in the pod spec. The mounts keep running while the driver is upgraded or
restarted. rclone runs as root on the node and many of its options reach local files or
run commands, so options set as `volumeAttributes` or in a Secret are checked against an
allow-list, and only admins write the `rclone-secret`.

This is a fork of [wunderio/csi-rclone](https://github.com/wunderio/csi-rclone), which
starts `rclone mount --daemon` in the node plugin container and passes volume attributes
to it unchecked. The fork keeps its volume attributes, `rclone-secret` and StorageClass.

## Contents
- [Features](#features)
- [Requirements](#requirements)
- [Install](#install)
- [Configuration](#configuration): [rclone-secret and volume attributes](#rclone-secret-and-volume-attributes), [Static PersistentVolumes](#static-persistentvolumes), [Dynamic provisioning](#dynamic-provisioning), [Ephemeral volumes](#ephemeral-volumes), [rclone option allow-list](#rclone-option-allow-list), [Chart values](#chart-values)
- [Mount lifetime](#mount-lifetime): [Dead mounts](#dead-mounts)
- [Security](#security)
- [How it works](#how-it-works)
- [Development](#development)

## Features

- **Mounts survive driver restarts and upgrades.** Each rclone runs in a host systemd
  scope, and the node plugin keeps its state on disk. See [Mount lifetime](#mount-lifetime).
- **Static, dynamic and ephemeral volumes.** PersistentVolumes, a StorageClass that
  provisions a per-PVC path, and inline CSI volumes.
- **One rclone per remote for ephemeral volumes.** Ephemeral volumes with exactly the same
  rclone arguments, credentials included, share one rclone mount per node, and with it its
  VFS cache (pods see each other's writes before they are uploaded) and connection.
  `reuseMounts: false` gives each volume its own. Each PersistentVolume has its own mount
  at the kubelet staging path.
- **rclone option allow-list.** Options that could read local files, run commands or use
  the node's cloud identity are refused; endpoints can be limited to a set of hosts.
- **Per-mount resource limits.** `MemoryMax` and `TasksMax` for each rclone scope.
- **Plain CSI behavior.** A dead mount is replaced when the volume is staged again, not
  repaired behind the back of its pods, see [Dead mounts](#dead-mounts).

## Requirements

- Kubernetes 1.25 or newer. Driver versions before v3.0.0 support Kubernetes 1.13-1.19,
  but are not maintained.
- The `fuse` kernel module on the nodes.
- Host systemd on the nodes for mounts to survive driver restarts (optional, see
  [Mount lifetime](#mount-lifetime)).
- A namespace that admits privileged pods with host network and, unless `daemonLifetime`
  is `in-container`, host PID.
- On MicroK8s, `kubeletRootDir` set to `/var/snap/microk8s/common/var/lib/kubelet`.
- On Ubuntu 26.04 hosts, an AppArmor override for `fusermount3`, whose distribution
  profile denies mounts below `/var/lib/csi-rclone` and the kubelet staging path (pods
  stay in `ContainerCreating` with `fusermount3: mount failed: Permission denied`). On
  each node, add to `/etc/apparmor.d/local/fusermount3`, with `kubeletRootDir` in place
  of `/var/lib/kubelet` if it differs:

  ```
  mount fstype=fuse.rclone -> /var/lib/csi-rclone/**,
  mount fstype=fuse.rclone -> /var/lib/kubelet/plugins/kubernetes.io/csi/**,
  ```

  and reload with `apparmor_parser -r /etc/apparmor.d/fusermount3`.

## Install

```sh
helm repo add csi-rclone https://ruakij.github.io/csi-rclone
helm repo update
helm install csi-rclone csi-rclone/csi-rclone --namespace csi-rclone --create-namespace
```

Or straight from the OCI registry, which is also where prereleases go:

```sh
helm install csi-rclone oci://ghcr.io/ruakij/charts/csi-rclone --namespace csi-rclone --create-namespace
```

Or from a checkout, to run an unreleased revision: `helm install csi-rclone charts/csi-rclone ...`.
The driver name is `csi-rclone`; PersistentVolumes and pods put it in `csi.driver`.

## Configuration

### rclone-secret and volume attributes

The Secret `rclone-secret` in the release namespace holds defaults for every
PersistentVolume, static or provisioned; the chart creates it from
`rcloneSecret.stringData` with `rcloneSecret.create: true`. Its keys, the
`nodePublishSecretRef` Secret (ephemeral volumes) and `volumeAttributes` are merged,
the latter winning:

- `remote` (required): an rclone backend such as `s3`, or a section name in `configData`.
- `remotePath` (required): path on the remote, such as a bucket and prefix.
- `configData`: rclone configuration file contents, see
  [rclone-secret-file-config.yaml](example/kubernetes/rclone-secret-file-config.yaml).
- other keys: rclone flags without `--`, e.g. `s3-endpoint`, within the allow-list.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: rclone-secret
type: Opaque
stringData:
  remote: "s3"
  remotePath: "projectname"
  s3-provider: "Minio"
  s3-endpoint: "http://minio.minio:9000"
  s3-access-key-id: "ACCESS_KEY_ID"
  s3-secret-access-key: "SECRET_ACCESS_KEY"
```

rclone mounts a volume read-only when the access mode is `ReadOnlyMany` or the
PersistentVolume has `ro` in `mountOptions`. A pod that mounts it with `readOnly: true`
gets a read-only bind of a writable mount. Other `mountOptions` are ignored.

### Static PersistentVolumes

Leave `volumeAttributes` empty to use `rclone-secret` alone. See
[nginx-example.yaml](example/kubernetes/nginx-example.yaml).

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: data-rclone-example
spec:
  accessModes: [ReadWriteMany]
  capacity:
    storage: 10Gi
  # Only this PVC can bind the PV, and no provisioner uses the empty class
  claimRef:
    namespace: default
    name: data-rclone-example
  storageClassName: ""
  csi:
    driver: csi-rclone
    volumeHandle: data-id
    volumeAttributes:
      remote: "s3"
      remotePath: "projectname/pvname"
      s3-access-key-id: "ACCESS_KEY_ID"        # overrides rclone-secret
      s3-secret-access-key: "SECRET_ACCESS_KEY"
```

### Dynamic provisioning

With `storageClass.create: true`, the chart creates the StorageClass `rclone`. Its
volumes use `remote` and `remotePath` from `rclone-secret`, plus a per-PVC suffix from
the StorageClass parameter `pathPattern`, built from `${.PVC.namespace}`,
`${.PVC.name}`, `${.PVC.labels.<key>}` and `${.PVC.annotations.<key>}`. See
[nginx-pvc-example.yaml](example/kubernetes/nginx-pvc-example.yaml).

`pathPattern` must contain `${.PVC.namespace}` as a whole path segment, with only fixed
text before it (e.g. `${.PVC.namespace}/${.PVC.name}`), otherwise PVCs in different
namespaces could mount each other's data and provisioning fails. The parameter
`sharedRemote: "true"` disables that check for classes whose PVCs share data. Expanded
paths with `.` or `..` segments are rejected.

PVC annotations: `csi-rclone/umask` sets the octal `umask` of `rclone mount`;
`csi-rclone/storage-path` fills `${.PVC.annotations.csi-rclone/storage-path}`. Other
parameters are not provisioned; create a PersistentVolume for them. Deleting a PVC does
not delete its data on the remote; a new PVC with the same path gets it.

### Ephemeral volumes

Pods can define a volume inline, without a PersistentVolume. `rclone-secret` is not
applied, since anyone who can create pods could otherwise mount any path with its
credentials. Credentials come from `configData`, `volumeAttributes` or a Secret in the
pod's namespace named by `nodePublishSecretRef`, whose keys become rclone flags and are
overridden by `volumeAttributes`.

```yaml
volumes:
  - name: data
    csi:
      driver: csi-rclone
      volumeAttributes:
        remote: s3
        remotePath: bucket/path
        s3-provider: Minio
        s3-endpoint: http://minio.minio:9000
      nodePublishSecretRef:
        name: s3-credentials # s3-access-key-id, s3-secret-access-key
```

`ephemeralVolumes: false` disables them in the CSIDriver.

### rclone option allow-list

The node plugin rejects volumes with rclone options that could read or write local
files, run commands, reach local sockets or use the node's cloud identity. It allows:

- the backends `s3`, `gcs`, `azureblob`, `b2`, `swift`, `webdav`, `sftp`, `ftp`,
  `drive`, `onedrive`, `dropbox` and `crypt`, set as `remote` or as `type` in
  `configData`,
- their options from `pkg/rclone/options_gen.go`, which excludes file, path and command
  options and ambient credentials like `env_auth`,
- mount, VFS, filter and network tuning flags, but not `cache-dir`, `log-file` or
  `config`,
- `crypt` remotes wrapping a `configData` remote or `:type:path`, but no local path,
  connection string or other `crypt` remote.

`options_gen.go` is generated for the rclone version in the image:
`rclone config providers | go run ./hack/gen-options v1.76.0 > pkg/rclone/options_gen.go`.
Review the diff after an rclone update.

### Chart values

The values that matter; [values.yaml](charts/csi-rclone/values.yaml) has the rest
(images, resources, RBAC, ServiceAccounts, scheduling, `logLevel`).

| Value                       | Default            | Description                                                                                                          |
| --------------------------- | ------------------ | -------------------------------------------------------------------------------------------------------------------- |
| `daemonLifetime`            | `auto`             | `auto` uses host systemd where reachable, `systemd` requires it, `in-container` keeps rclone in the node plugin pod. |
| `scopeMemoryMax`            | `""`               | `MemoryMax` of each rclone scope, e.g. `2Gi`.                                                                        |
| `scopeTasksMax`             | `0`                | `TasksMax` of each rclone scope, `0` for no limit.                                                                   |
| `allowedBackends`           | `[]`               | Restricts the backends further, e.g. `[s3, crypt]`. Empty allows all of the allow-list.                              |
| `allowedEndpoints`          | `[]`               | Hosts that endpoint options may point at; entries starting with `.` allow subdomains. Empty allows any.              |
| `unrestrictedRcloneOptions` | `false`            | Allows every rclone option and backend, see [Security](#security).                                                   |
| `ephemeralVolumes`          | `true`             | Allow inline CSI volumes. Immutable in the CSIDriver: changing it needs the CSIDriver deleted first.                 |
| `reuseMounts`               | `true`             | Share one rclone mount between ephemeral volumes with exactly the same rclone arguments.                             |
| `storageClass.create`       | `false`            | Create the StorageClass; `storageClass.{name,pathPattern,sharedRemote,reclaimPolicy}` configure it.                  |

`node.resources` sets no limits on purpose: with `in-container`, a limit would OOM-kill
every mount on the node. The node plugin flags behind these values are
`--daemon-lifetime`, `--scope-memory-max`, `--scope-tasks-max`, `--allowed-backends`,
`--allowed-endpoints`, `--unrestricted-rclone-options` and `--reuse-mounts`. rclone logs
to `/var/lib/csi-rclone/<hash>/rclone.log` on the node.

## Mount lifetime

Each rclone is moved into a transient host systemd scope, `csi-rclone-<hash>.scope`, so
it belongs to host systemd, not the driver pod's cgroup. With `daemonLifetime`:

- `systemd`, or `auto` with host systemd: node plugin restarts leave the mounts alone.
  `systemd` makes the node plugin refuse to start on nodes without host systemd.
- `in-container`, or `auto` without systemd: mounts die with the pod and stay dead until
  they are staged again.

Surviving rclone processes are never restarted for an upgrade, since that would break
their open file descriptors: they keep their rclone version until the volume is
unmounted.

A PersistentVolume is mounted at the kubelet staging path, as in other CSI drivers, and
an ephemeral volume under `/var/lib/csi-rclone/<hash>/mnt`. Mounts staged by 4.1.0 to
4.2.x are under `/var/lib/csi-rclone/<hash>/mnt` for PersistentVolumes too, possibly
shared, with an empty staging path; they keep that layout until unstaged.

### Dead mounts

An rclone crash or OOM kill leaves its mount dead in every mode. The node plugin learns
of it from its child processes or the stopped scope, logs it in a pass every 30s and
drops targets that kubelet removed without unpublishing them.

A dead mount is not repaired in place. It is replaced when a CSI caller stages the
volume again: `NodeStageVolume` mounts it anew at the same path (the staging path for a
PersistentVolume), whether the volume is unstaged first or still staged. A volume staged
by 4.1.0 to 4.2.x gets the staging path layout when it is unstaged and staged again.
Kubelet stages volumes after a node reboot. Running pods keep their copy of the dead
mount and see `Transport endpoint is not connected` until their volume paths are
published again, which takes a pod restart or a healer.
[csi-mount-healer](https://github.com/Ruakij/csi-mount-healer) is the recommended
companion: it unstages and stages the volume and publishes it to each pod again.
`NodePublishVolume` fails with `FailedPrecondition` for a staged volume that is not
mounted, so a new pod waits until the volume is staged again.

A running rclone is never stopped or replaced, since the copies of its mount in the
containers live and die with it. If its mount does not answer or is not mounted, the
node plugin logs `leaving it alone` and staging and publishing the volume fail
meanwhile. Probing a mount point times out after 5s.

## Security

- rclone runs as root in the privileged node plugin, which also uses `hostPID` and the
  host systemd D-Bus socket (host root). Keys in `rclone-secret` and PersistentVolume
  `volumeAttributes` become rclone flags and `configData` the rclone config, limited by
  the [rclone option allow-list](#rclone-option-allow-list). With
  `unrestrictedRcloneOptions`, rclone can run commands (`password-command`, `sftp-ssh`),
  write files (`log-file`) and mount local paths (`local` backend), so whoever can write
  `rclone-secret` or create PersistentVolumes has root on every node.
- The allow-list does not stop rclone from connecting to any host, including cloud
  metadata endpoints and cluster-internal services. Set `allowedEndpoints`, and block
  these hosts with a NetworkPolicy or firewall, since rclone follows redirects and
  resolves DNS itself.
- With ephemeral volumes, anyone who can create pods sets rclone options, within the
  allow-list. Set `ephemeralVolumes: false`, or restrict inline `csi-rclone` volumes
  with an admission policy, if that is not wanted.
- Static PersistentVolumes should set `claimRef`, otherwise a PVC in any namespace can
  bind them and use their credentials.
- `/var/lib/csi-rclone` on each node holds every mounted volume's rclone config and
  flags, including credentials, readable only by root.

## How it works

The chart installs a controller, which provisions volumes for the StorageClass, and a
node plugin DaemonSet. For each volume on a node, the node plugin:

1. Merges `rclone-secret`, `nodePublishSecretRef` and `volumeAttributes` into a remote,
   a path, an optional rclone config and rclone flags, and checks them against the
   [rclone option allow-list](#rclone-option-allow-list).
2. Starts `rclone mount` and moves it into its systemd scope. A PersistentVolume is
   mounted at the kubelet staging path. An ephemeral volume looks for a running rclone
   mount with exactly these arguments, credentials and read-only mode included, and
   otherwise mounts at `/var/lib/csi-rclone/<hash>/mnt`.
3. Records the volume as a user of that mount in `/var/lib/csi-rclone/<hash>/state.json`
   and bind-mounts the mount into the pod, read-only if the pod asks for it.
4. Waits for pending uploads and unmounts when the last user is unstaged or unpublished.

Kubelet stages PersistentVolumes and publishes them to pods in two steps; ephemeral
volumes are only published, and the node plugin stages them itself.

## Development

```sh
git config core.hooksPath .githooks  # go mod tidy, gofmt, vet, tests, golangci-lint, helm lint before each commit
make build     # image tagged with the version in VERSION
make e2e       # chart on a kind cluster, needs docker, kind, kubectl and helm; E2E_KEEP=1 keeps the cluster
go test ./...  # unit tests and, on Linux, the csi-test sanity suite
```

Mount code is Linux-only; on a non-Linux machine, build and vet with `GOOS=linux`.

Pushing a tag `vX.Y.Z` publishes the image to `ghcr.io/ruakij/csi-rclone`, the chart to
the Helm repository and `oci://ghcr.io/ruakij/charts`, and a GitHub release. Tags with a
`-` suffix are prereleases.
