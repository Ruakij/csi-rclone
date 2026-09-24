# csi-rclone

[![Helm](https://img.shields.io/badge/dynamic/yaml?url=https%3A%2F%2Fruakij.github.io%2Fcsi-rclone%2Findex.yaml&query=%24.entries%5B%27csi-rclone%27%5D%5B0%5D.version&label=Helm&logo=helm&color=0F1689&prefix=v)](#install)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25%2B-326CE5?logo=kubernetes&logoColor=white)](#requirements)

**An rclone mount CSI driver for Kubernetes.**

It mounts any remote [rclone](https://rclone.org/) supports, S3, GCS, Azure Blob,
WebDAV, SFTP and more, as a pod volume through
[`rclone mount`](https://rclone.org/commands/rclone_mount/). Volumes come from static
PersistentVolumes, from a StorageClass that gives each PVC its own path on one remote,
or inline in the pod spec.

rclone options are set as `volumeAttributes` or in a Secret, and checked against an
allow-list, since rclone runs as root on the node and many of its options reach local
files or run commands.

## Features

- **Mounts survive driver restarts and upgrades.** Each rclone runs in a host systemd
  scope, not in the driver pod. See [Surviving restarts](#surviving-restarts).
- **Dead mounts come back.** When an rclone dies, the node plugin remounts it within a
  second and binds it into the pods again.
- **Static, dynamic and ephemeral volumes.** PersistentVolumes with their own
  credentials, a StorageClass that provisions a per-PVC path, and inline CSI volumes in
  pod specs.
- **One rclone per remote.** Volumes with exactly the same rclone arguments,
  credentials included, share one running rclone mount, whether they are
  PersistentVolumes or ephemeral and whether or not they are related in Kubernetes.
- **rclone option allow-list.** Backends and options that could read local files, run
  commands or use the node's cloud identity are refused, and endpoints can be limited
  to a set of hosts.
- **Namespace-safe provisioning.** A StorageClass path pattern must separate
  namespaces, so PVCs cannot reach each other's data.
- **Per-mount resource limits.** `MemoryMax` and `TasksMax` for each rclone scope,
  outside the node plugin's own limits.

## Differences from upstream

This is a fork of [wunderio/csi-rclone](https://github.com/wunderio/csi-rclone), which
starts `rclone mount --daemon` in the node plugin container at each pod's volume path
and passes volume attributes to it unchecked. The fork keeps its volume attributes,
`rclone-secret` and StorageClass, and changes the approach:

- **Volume attributes are untrusted input.** Whoever creates PersistentVolumes or pods
  should not get root on the node through rclone options, so options are allowed by
  name instead of passed through, and pod creators never get the admin's
  `rclone-secret`.
- **Mounts are owned by the node, not the driver pod.** rclone runs in host systemd
  scopes and the node plugin keeps its state on disk, so a restart or upgrade of the
  driver finds its mounts instead of losing track of them.
- **One rclone per remote, not per pod.** rclone mounts once per node at a staging
  path and is bind-mounted into each pod, so pods on a node share one VFS cache and
  one connection.
- **Failures are repaired, not left to the user.** Dead mounts are remounted and bound
  into their pods again; nothing is left behind when the last user goes.
- **Shipped and tested as a whole.** A Helm chart and versioned images from tagged
  releases, the csi-test sanity suite, and end-to-end tests on a kind cluster in CI.

## Requirements

- Kubernetes 1.25 or newer. Driver versions before v3.0.0 support Kubernetes 1.13-1.19,
  but are not maintained.
- The `fuse` kernel module on the nodes.
- Host systemd on the nodes for mounts to survive driver restarts. Optional, see
  [Surviving restarts](#surviving-restarts).
- On MicroK8s, `kubeletRootDir` set to the node's real kubelet directory,
  `/var/snap/microk8s/common/var/lib/kubelet`.
- A namespace that admits privileged pods with host network and, unless
  `daemonLifetime` is `in-container`, host PID.

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

Or from a checkout, to run an unreleased revision:

```sh
helm install csi-rclone charts/csi-rclone --namespace csi-rclone --create-namespace
```

The driver name is `csi-rclone`, and that is what PersistentVolumes and pods put in
`csi.driver`.

## Use

A remote needs a backend rclone supports, such as [MinIO](https://min.io/) or any
S3-compatible service.

### Defaults in rclone-secret

The Secret `rclone-secret` in the release namespace holds defaults for every
PersistentVolume, static or provisioned. It is optional if PersistentVolumes set
everything in `volumeAttributes`. The chart creates it from `rcloneSecret.stringData`
with `rcloneSecret.create: true`.

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

Or with an rclone configuration file in `configData`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: rclone-secret
type: Opaque
stringData:
  remote: "my-s3"
  remotePath: "projectname"
  configData: |
    [my-s3]
    type = s3
    provider = Minio
    access_key_id = ACCESS_KEY_ID
    secret_access_key = SECRET_ACCESS_KEY
    endpoint = http://minio-release.default:9000
```

See [rclone-secret-example.yaml](example/kubernetes/rclone-secret-example.yaml) and
[rclone-secret-file-config.yaml](example/kubernetes/rclone-secret-file-config.yaml).

### Static PersistentVolumes

Keys in `volumeAttributes` override those in `rclone-secret`. Leave `volumeAttributes`
empty to use the Secret alone.

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: data-rclone-example
spec:
  accessModes:
    - ReadWriteMany
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
      s3-provider: "Minio"
      s3-endpoint: "http://minio.minio:9000"
      s3-access-key-id: "ACCESS_KEY_ID"
      s3-secret-access-key: "SECRET_ACCESS_KEY"
```

See [nginx-example.yaml](example/kubernetes/nginx-example.yaml).

### Dynamic provisioning

With `storageClass.create: true`, the chart creates the StorageClass `rclone`. Its
volumes use `remote` and `remotePath` from `rclone-secret`, with a per-PVC suffix from
`pathPattern`. See [nginx-pvc-example.yaml](example/kubernetes/nginx-pvc-example.yaml)
and [StorageClass parameters](#storageclass-parameters).

Deleting a PVC does not delete its data on the remote. A new PVC that expands to the
same path gets the old data.

### Ephemeral volumes

Pods can define a volume inline, without a PersistentVolume. Its `volumeAttributes` are
the same as a PersistentVolume's. `rclone-secret` is not applied, since anyone who can
create pods could otherwise mount any path with its credentials. Credentials come from
`configData`, `volumeAttributes` or a Secret in the pod's namespace named by
`nodePublishSecretRef`, whose keys become rclone flags like `volumeAttributes`, which
override them.

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

The chart value `ephemeralVolumes: false` disables ephemeral volumes in the CSIDriver.

### Shared mounts

A volume reuses the running rclone mount of any other volume on the node whose
`remote`, `remotePath`, `configData`, flags and read-only mode match it exactly,
credentials included. The volumes need not be related in Kubernetes: a
PersistentVolume and an ephemeral volume on the same remote share one rclone. They
also share its VFS cache, so each sees the other's writes before they are uploaded.
`reuseMounts: false` gives each volume its own rclone.

## Configuration

### Volume attributes

Set in `volumeAttributes`, `rclone-secret` or the `nodePublishSecretRef` Secret:

| Key          | Default  | Description                                                                             |
| ------------ | -------- | --------------------------------------------------------------------------------------- |
| `remote`     | required | rclone remote: a backend such as `s3`, or a section name in `configData`.               |
| `remotePath` | required | Path on the remote, such as a bucket and prefix.                                        |
| `configData` | none     | rclone configuration file contents.                                                     |
| other keys   | none     | rclone flags without `--`, e.g. `s3-endpoint`, `vfs-cache-mode`, within the allow-list. |

rclone mounts a volume read-only when the access mode is `ReadOnlyMany` or the
PersistentVolume has `ro` in `mountOptions`. A pod that mounts it with `readOnly: true`
gets a read-only bind of a writable mount. Other `mountOptions` are ignored; set rclone
flags in `volumeAttributes`.

### StorageClass parameters

Dynamically provisioned volumes all use `remote` and `remotePath` from `rclone-secret`.

| Parameter      | Description                                                                                                                              |
| -------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `pathPattern`  | Suffix appended to `remotePath`, built from `${.PVC.namespace}`, `${.PVC.name}`, `${.PVC.labels.<key>}` and `${.PVC.annotations.<key>}`. |
| `sharedRemote` | `"true"` disables the namespace check below, for StorageClasses where all PVCs are meant to share data.                                  |

`pathPattern` must contain `${.PVC.namespace}` as a whole path segment, with only fixed
text before it (e.g. `${.PVC.namespace}/${.PVC.name}`). Otherwise PVCs in different
namespaces could mount each other's data, and provisioning fails. Expanded paths
containing `.` or `..` segments are rejected.

### PersistentVolumeClaim annotations

| Annotation                | Description                                                                               |
| ------------------------- | ----------------------------------------------------------------------------------------- |
| `csi-rclone/umask`        | `umask` parameter for `rclone mount`, octal.                                              |
| `csi-rclone/storage-path` | Path segment for `${.PVC.annotations.csi-rclone/storage-path}`, if `pathPattern` uses it. |

Provisioning of other parameters is unsupported; create a PersistentVolume with
`volumeAttributes` to set them.

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
`rclone config providers | go run ./hack/gen-options v1.75.1 > pkg/rclone/options_gen.go`.
Review the diff after an rclone update.

### Chart values

| Value                                                | Default                                                 | Description                                                                                                          |
| ---------------------------------------------------- | ------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| `image.repository`                                   | `ghcr.io/ruakij/csi-rclone`                             | Driver image.                                                                                                        |
| `image.tag`                                          | chart `appVersion`                                      | Driver image tag.                                                                                                    |
| `image.pullPolicy`                                   | `IfNotPresent`                                          | Driver image pull policy.                                                                                            |
| `registrar.image.*`                                  | `registry.k8s.io/sig-storage/csi-node-driver-registrar` | `node-driver-registrar` image, same fields as `image`.                                                               |
| `registrar.resources`                                | requests `5m` CPU, `16Mi` memory                        | `node-driver-registrar` container resources.                                                                         |
| `provisioner.image.*`                                | `registry.k8s.io/sig-storage/csi-provisioner`           | `csi-provisioner` image, same fields as `image`.                                                                     |
| `provisioner.resources`                              | requests `5m` CPU, `32Mi` memory                        | `csi-provisioner` container resources.                                                                               |
| `attacher.image.*`                                   | `registry.k8s.io/sig-storage/csi-attacher`              | `csi-attacher` image, same fields as `image`.                                                                        |
| `attacher.resources`                                 | requests `5m` CPU, `32Mi` memory                        | `csi-attacher` container resources.                                                                                  |
| `kubeletRootDir`                                     | `/var/lib/kubelet`                                      | The node's kubelet directory. MicroK8s uses `/var/snap/microk8s/common/var/lib/kubelet`.                             |
| `daemonLifetime`                                     | `auto`                                                  | `auto` uses host systemd where reachable, `systemd` requires it, `in-container` keeps rclone in the node plugin pod. |
| `scopeMemoryMax`                                     | `""`                                                    | `MemoryMax` of each rclone scope, e.g. `2Gi`. rclone does not count against the node plugin's resource limits.       |
| `scopeTasksMax`                                      | `0`                                                     | `TasksMax` of each rclone scope, `0` for no limit.                                                                   |
| `allowedBackends`                                    | `[]`                                                    | Restricts the backends further, e.g. `[s3, crypt]`. Empty allows all of the allow-list.                              |
| `allowedEndpoints`                                   | `[]`                                                    | Hosts that endpoint options may point at. Entries starting with `.` allow subdomains. Empty allows any.              |
| `unrestrictedRcloneOptions`                          | `false`                                                 | Allows every rclone option and backend, see [Security](#security).                                                   |
| `ephemeralVolumes`                                   | `true`                                                  | Allow inline CSI volumes in pod specs. Immutable in the CSIDriver: changing it needs the CSIDriver deleted first.    |
| `reuseMounts`                                        | `true`                                                  | Share one rclone mount between volumes with exactly the same rclone arguments.                                       |
| `rcloneSecret.create`                                | `false`                                                 | Create `rclone-secret` from `rcloneSecret.stringData`.                                                               |
| `rcloneSecret.stringData`                            | `{}`                                                    | Defaults for every PersistentVolume, see [Defaults in rclone-secret](#defaults-in-rclone-secret).                    |
| `storageClass.create`                                | `false`                                                 | Create a StorageClass for dynamic provisioning.                                                                      |
| `storageClass.name`                                  | `rclone`                                                | StorageClass name.                                                                                                   |
| `storageClass.pathPattern`                           | `${.PVC.namespace}/${.PVC.name}`                        | Per-PVC path suffix, see [StorageClass parameters](#storageclass-parameters).                                        |
| `storageClass.sharedRemote`                          | `false`                                                 | Let all PVCs of the class share data.                                                                                |
| `storageClass.reclaimPolicy`                         | `Delete`                                                | StorageClass reclaim policy. Data on the remote is kept either way.                                                  |
| `logLevel`                                           | `1`                                                     | klog verbosity.                                                                                                      |
| `rbac.create`                                        | `true`                                                  | Create the ClusterRoles and bindings.                                                                                |
| `serviceAccount.create`                              | `true`                                                  | Create the ServiceAccounts.                                                                                          |
| `serviceAccount.controllerName`                      | `""`                                                    | Controller ServiceAccount; defaults to one derived from the release name.                                            |
| `serviceAccount.nodeName`                            | `""`                                                    | Node plugin ServiceAccount; defaults to one derived from the release name.                                           |
| `controller.replicas`                                | `1`                                                     | Controller replicas.                                                                                                 |
| `controller.resources`                               | requests `10m` CPU, `32Mi` memory                       | Controller container resources.                                                                                      |
| `controller.nodeSelector`, `tolerations`, `affinity` | `{}`, `[]`, `{}`                                        | Controller scheduling constraints.                                                                                   |
| `node.priorityClassName`                             | `system-node-critical`                                  | Priority class of the node plugin pods.                                                                              |
| `node.tolerations`                                   | tolerate everything                                     | Node plugin tolerations.                                                                                             |
| `node.nodeSelector`, `affinity`                      | `{}`                                                    | Node plugin scheduling constraints.                                                                                  |
| `node.resources`                                     | requests `10m` CPU, `64Mi` memory                       | Node plugin container resources. No limits: with `in-container`, a limit would OOM-kill every mount on the node.     |
| `node.updateStrategy`                                | `RollingUpdate`                                         | Node plugin DaemonSet update strategy.                                                                               |
| `podAnnotations`, `podLabels`                        | `{}`                                                    | Extra metadata on all pods.                                                                                          |

The node plugin flags behind these values are `--daemon-lifetime`,
`--scope-memory-max`, `--scope-tasks-max`, `--allowed-backends`, `--allowed-endpoints`,
`--unrestricted-rclone-options` and `--reuse-mounts`. rclone logs to
`/var/lib/csi-rclone/<hash>/rclone.log` on the node.

## How it works

The chart installs a controller, which provisions volumes for the StorageClass, and a
node plugin DaemonSet. For each volume on a node, the node plugin:

1. Merges `rclone-secret` (PersistentVolumes only), the `nodePublishSecretRef` Secret
   (ephemeral volumes only) and `volumeAttributes`, later ones winning, into a remote,
   a path, an optional rclone config and rclone flags.
2. Checks them against the [rclone option allow-list](#rclone-option-allow-list).
3. Looks for a running rclone mount with exactly these arguments, credentials and
   read-only mode included. If there is none, it starts `rclone mount` at
   `/var/lib/csi-rclone/<hash>/mnt` and moves it into its systemd scope.
4. Records the volume as a user of that mount in `/var/lib/csi-rclone/<hash>/state.json`
   and bind-mounts the mount into the pod, read-only if the pod asks for it.

Kubelet stages PersistentVolumes and publishes them to pods in two steps; ephemeral
volumes are only published, and the node plugin stages them itself. When the last
volume using a mount is unstaged or unpublished, the node plugin waits for pending
uploads and unmounts it.

### Surviving restarts

Many FUSE-based CSI drivers run their FUSE daemons inside the driver pod. Restarting,
upgrading or evicting that pod kills every daemon on the node, and every workload using
those mounts is left with `Transport endpoint is not connected` until it is recreated.

Here, each rclone is moved into a transient host systemd scope,
`csi-rclone-<hash>.scope`, the same mechanism as `systemd-run --scope`. From then on it
belongs to host systemd, not the driver pod's cgroup.

| `daemonLifetime`              | Node plugin restarts                                                          | rclone crash or OOM kill                                         |
| ----------------------------- | ----------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| `systemd`, or `auto` with it  | Unaffected: rclone belongs to host systemd.                                   | Remounted within a second; open file descriptors see `ENOTCONN`. |
| `in-container`, or no systemd | Remounted once the node plugin is back; open file descriptors see `ENOTCONN`. | Remounted within a second; open file descriptors see `ENOTCONN`. |

The node plugin learns of exits from its own child processes and, with host systemd,
from the scope stopping, which also covers rclone processes started by an earlier node
plugin pod. A pass every 30s catches anything missed. The mount comes back at the same
path, and is bound into the pods' volume paths again, but running containers keep
seeing `Transport endpoint is not connected` on the old file descriptors until they
reopen them or restart.

Surviving rclone processes are never restarted for an upgrade, since that would break
their open file descriptors: they keep their rclone version until the volume is
unmounted.

With `daemonLifetime: systemd`, the node plugin does not start at all on nodes without
host systemd, instead of silently falling back.

## Security

- rclone runs as root in the privileged node plugin. Keys in `rclone-secret` and
  PersistentVolume `volumeAttributes` become rclone flags, and `configData` becomes the
  rclone config, both limited by the [rclone option allow-list](#rclone-option-allow-list).
  With `unrestrictedRcloneOptions`, rclone can run commands (`password-command`,
  `sftp-ssh`), write files (`log-file`) and mount local paths (`local` backend), so
  whoever can write `rclone-secret` or create PersistentVolumes has root on every node.
- The allow-list does not stop rclone from connecting to any host, including cloud
  metadata endpoints and cluster-internal services. Set `allowedEndpoints`, and block
  these hosts with a NetworkPolicy or firewall, since rclone follows redirects and
  resolves DNS itself.
- With ephemeral volumes, anyone who can create pods sets rclone options, within the
  allow-list. Set `ephemeralVolumes: false`, or restrict inline `csi-rclone` volumes
  with an admission policy, if that is not wanted.
- Static PersistentVolumes should set `claimRef`, otherwise a PVC in any namespace can
  bind them and use their credentials.
- The node plugin uses `hostPID` and the host systemd D-Bus socket to start rclone
  scopes, which is host root as well.
- Volumes that share an rclone mount also share its VFS cache, so pods see each other's
  writes before they are uploaded.
- `/var/lib/csi-rclone` on each node holds every mounted volume's rclone config and
  flags, including credentials, readable only by root.

## Build

```sh
make build  # image tagged with the version in VERSION
make e2e    # chart on a kind cluster, needs docker, kind, kubectl and helm
```

Mount code is Linux-only; on a non-Linux machine, build and vet with `GOOS=linux`.

`go test ./...` runs the unit tests, and on Linux the csi-test sanity suite. `make e2e`
installs the chart into a [kind](https://kind.sigs.k8s.io) cluster and checks writes,
read-only volumes, the allow-list, driver restarts, remounts, shared mounts and cleanup
from real pods; `E2E_KEEP=1` leaves the cluster running.

`git config core.hooksPath .githooks` enables the pre-commit hook, which runs
`go mod tidy`, gofmt, vet, tests and golangci-lint on the staged packages, and
`helm lint` when the chart changed.

Pushing a tag `vX.Y.Z` publishes the image to `ghcr.io/ruakij/csi-rclone`, the chart to
the Helm repository and `oci://ghcr.io/ruakij/charts`, and a GitHub release. Tags with a
`-` suffix are prereleases.
