
# CSI rclone mount plugin

This project implements Container Storage Interface (CSI) plugin that allows using [rclone mount](https://rclone.org/) as storage backend. Rclone mount points and [parameters](https://rclone.org/commands/rclone_mount/) can be configured using Secret or PersistentVolume volumeAttibutes. 

## Kubernetes cluster compatability
Works (tested):
- The Helm chart requires Kubernetes 1.25 or newer.
- Older driver versions (before v3.0.0) support kubernetes 1.13-1.19, but are not maintained.

## Installing CSI driver to kubernetes cluster
TLDR: `helm install csi-rclone oci://ghcr.io/ruakij/charts/csi-rclone --namespace csi-rclone --create-namespace`

The chart can create `rclone-secret` (`rcloneSecret.create`, `rcloneSecret.stringData`) and a StorageClass (`storageClass.create`), and sets the node plugin flags below from its values, see [values.yaml](charts/csi-rclone/values.yaml).

1. Set up storage backend. You can use [Minio](https://min.io/), Amazon S3 compatible cloud storage service.
i.e (heads up - minio setup example is severly outdated). 
```
helm upgrade --install --create-namespace --namespace minio minio minio/minio --version 6.0.5 --set resources.requests.memory=512Mi --set secretKey=SECRET_ACCESS_KEY --set accessKey=ACCESS_KEY_ID
```

2. Configure defaults by pushing secret `rclone-secret` to the `csi-rclone` namespace. This is optional if you will always define `volumeAttributes` in PersistentVolume.

```
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

Alternatively, you may specify rclone configuration file directly in the secret under `configData` field.

```
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

Deploy example secret
> `kubectl apply -f example/kubernetes/rclone-secret-example.yaml`

3. You can override configuration via PersistentStorage resource definition. Leave volumeAttributes empty if you don't want to. Keys in `volumeAttributes` will be merged with predefined parameters.

```
apiVersion: v1
kind: PersistentVolume
metadata:
  name: data-rclone-example
  labels:
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

Deploy example definition
> `kubectl apply -f example/kubernetes/nginx-example.yaml`


## StorageClass parameters

Dynamically provisioned volumes all use `remote` and `remotePath` from `rclone-secret`. `pathPattern` appends a per-PVC suffix to `remotePath`, built from `${.PVC.namespace}`, `${.PVC.name}`, `${.PVC.labels.<key>}` and `${.PVC.annotations.<key>}`.

- `pathPattern` must contain `${.PVC.namespace}` as a whole path segment, with only fixed text before it (e.g. `${.PVC.namespace}/${.PVC.name}`). Otherwise PVCs in different namespaces could mount each other's data, and provisioning fails.
- `sharedRemote: "true"` disables this check, for StorageClasses where all PVCs are meant to share data.
- Expanded paths containing `.` or `..` segments are rejected.
- Deleting a PVC does not delete its data on the remote. A new PVC that expands to the same path gets the old data.

## PersistentVolumeClaim annotations

- `csi-rclone/umask` - `umask` parameter for `rclone mount`.
- [if configured in storageclass `parameters.pathPattern`] `csi-rclone/storage-path` - path segment for `${.PVC.annotations.csi-rclone/storage-path}`.

Provisioning of other parameters is currently unsupported, create PersistentVolume resource with `volumeAttributes` to define them.

## Mount lifetime

Each volume is mounted by one rclone process per node, which all pods on that node share. The node plugin moves it into a transient host systemd scope, `csi-rclone-<hash>.scope`, so it survives restarts and upgrades of the node plugin. Running rclone processes keep their rclone version until the volume is unmounted.

The node plugin remounts a volume when its rclone dies, and binds it into the pods' volume paths again. Running containers keep seeing `Transport endpoint is not connected` until they restart.

Node plugin flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--daemon-lifetime` | `auto` | `systemd` requires host systemd, `in-container` keeps rclone in the node plugin container, where it dies with it. `auto` uses systemd if it is reachable. |
| `--scope-memory-max` | none | `MemoryMax` of each rclone scope, e.g. `2Gi`. rclone does not count against the node plugin's resource limits. |
| `--scope-tasks-max` | none | `TasksMax` of each rclone scope. |

rclone logs to `/var/lib/csi-rclone/<hash>/rclone.log` on the node.

## rclone option allow-list

The node plugin rejects volumes with rclone options that could read or write local files, run commands, reach local sockets or use the node's cloud identity. It allows:

- the backends `s3`, `gcs`, `azureblob`, `b2`, `swift`, `webdav`, `sftp`, `ftp`, `drive`, `onedrive`, `dropbox` and `crypt`, set as `remote` or as `type` in `configData`,
- their options from `pkg/rclone/options_gen.go`, which excludes file, path and command options and ambient credentials like `env_auth`,
- mount, VFS, filter and network tuning flags, but not `cache-dir`, `log-file` or `config`,
- `crypt` remotes wrapping a `configData` remote or `:type:path`, but no local path, connection string or other `crypt` remote.

| Flag | Default | Description |
|------|---------|-------------|
| `--allowed-backends` | all above | Restricts the backends further, e.g. `--allowed-backends=s3,crypt`. |
| `--allowed-endpoints` | any | Hosts that endpoint options may point at. Entries starting with `.` allow subdomains. |
| `--unrestricted-rclone-options` | `false` | Allows every rclone option and backend. |

`options_gen.go` is generated for the rclone version in the image: `rclone config providers | go run ./hack/gen-options v1.75.1 > pkg/rclone/options_gen.go`. Review the diff after an rclone update.

## Security

- rclone runs as root in the privileged node plugin. Keys in `rclone-secret` and PersistentVolume `volumeAttributes` become rclone flags, and `configData` becomes the rclone config, both limited by the [rclone option allow-list](#rclone-option-allow-list). With `--unrestricted-rclone-options`, rclone can run commands (`password-command`, `sftp-ssh`), write files (`log-file`) and mount local paths (`local` backend), so whoever can write `rclone-secret` or create PersistentVolumes has root on every node.
- The allow-list does not stop rclone from connecting to any host, including cloud metadata endpoints and cluster-internal services. Set `--allowed-endpoints`, and block these hosts with a NetworkPolicy or firewall, since rclone follows redirects and resolves DNS itself.
- Static PersistentVolumes should set `claimRef`, otherwise a PVC in any namespace can bind them and use their credentials.
- rclone mounts a volume read-only when the access mode is `ReadOnlyMany` or the PersistentVolume has `ro` in `mountOptions`. A pod that mounts it with `readOnly: true` gets a read-only bind of a writable mount. Other `mountOptions` are ignored; set rclone flags in `volumeAttributes`.
- The node plugin uses `hostPID` and the host systemd D-Bus socket to start rclone scopes, which is host root as well.
- `/var/lib/csi-rclone` on each node holds every staged volume's rclone config and flags, including credentials, readable only by root.

## Development

```
git config core.hooksPath .githooks  # gofmt, go mod tidy, vet, tests, golangci-lint and helm lint before each commit
go test ./...                         # unit tests, and the csi-test sanity suite on Linux
make e2e                              # chart on a kind cluster, needs docker, kind, kubectl and helm; E2E_KEEP=1 keeps the cluster
make build                            # image tagged with the version in VERSION
```

Pushing a tag `vX.Y.Z` publishes the image to `ghcr.io/ruakij/csi-rclone`, the chart to `oci://ghcr.io/ruakij/charts` and a GitHub release. Tags with a `-` suffix are prereleases.

## Changelog

See [CHANGELOG.txt](CHANGELOG.txt)
