# Containerd snapshot image

This image supplies nerdctl 2.2.1, a shell, and CA certificates. The controller
runs it in a temporary Job on the source Pod's node, with the host containerd
socket and an emptyDir for nerdctl state. It does not run a second containerd.

Build both architectures from the repository root:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -f images/snapshot/Dockerfile \
  -t REGISTRY/devpod-snapshot:nerdctl-2.2.1 \
  --output type=oci,dest=devpod-snapshot.tar .
```

The build downloads the official architecture-specific nerdctl release and
checks its SHA256SUMS entry. No target-architecture command is executed during
the build. Publishing this image and a rebuilt controller image is a separate
deployment step. The example image names are not a claim that they are published.

## Controller configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `--snapshot-image` | `docker:cli` | Existing Docker snapshot tool image |
| `--snapshot-containerd-image` | `ghcr.io/mrhaoxx/devpod-snapshot:nerdctl-2.2.1` | Containerd tool image; set to the image you built |
| `--snapshot-containerd-address` | `/run/containerd/containerd.sock` | Absolute host socket path |
| `--snapshot-containerd-namespace` | `k8s.io` | Containerd namespace containing the source container |
| `--snapshot-containerd-insecure-registry` | `false` | Allow HTTP registries and skip TLS certificate verification in nerdctl |

For RKE2, set the address to `/run/k3s/containerd/containerd.sock`. These settings
belong to the controller deployment; users still create the same
`DevPodSnapshot` objects. The controller selects the backend from the source
Pod's `containerID` (`docker://` or `containerd://`).

Both backends commit the container filesystem and optionally push the result.
They use their CLI's default pause behavior. Volumes are not included. With
`push: true`, `status.digest` contains the pushed manifest digest; with
`push: false`, it retains the existing local image/config ID behavior. Containerd
push uses native image inspection (`.Image.Target.Digest`), not Docker's push-log
parser. Command failures are returned through the Job's termination message.

`pushSecretRef` supplies the same Docker-format registry credentials to both
backends. For containerd, the credentials are also mounted for commit-only Jobs,
because nerdctl may fetch missing source-image layers. The Job needs network
access to those source registries as well as the push destination. Nerdctl's
registry configuration is separate from RKE2's daemon configuration; the HTTP
flag is passed explicitly rather than inferred from the containerd socket.

References: [nerdctl registry configuration](https://github.com/containerd/nerdctl/blob/v2.2.1/docs/registry.md),
[commit implementation](https://github.com/containerd/nerdctl/blob/v2.2.1/pkg/imgutil/commit/commit.go),
[native image inspection type](https://github.com/containerd/nerdctl/blob/v2.2.1/pkg/inspecttypes/native/image.go).

## Render deployment configuration locally

The repository includes an RKE2 example with an HTTP registry at the
documentation address `192.0.2.10:5000`. Change the image tags and registry/network settings to match the intended
deployment, then render without contacting Kubernetes:

```sh
helm template devpod deploy/chart \
  -f deploy/examples/snapshot-rke2/values.yaml > /tmp/devpod-rke2.yaml
```

`snapshot.egress` optionally renders a NetworkPolicy selecting only the
controller's containerd snapshot Pods. The example permits DNS and
`192.0.2.10:5000`. Add the source registry addresses if they differ. It does
not change the network policy of user development Pods.

For an existing Kustomize deployment, copy
`deploy/examples/snapshot-rke2/patch-controller.yaml` into its overlay and add:

```yaml
patches:
  - path: patch-controller.yaml
    target:
      group: apps
      version: v1
      kind: Deployment
      name: devpod-controller
```

The patch updates the controller image and appends the containerd arguments;
existing Docker snapshot arguments remain. If the overlay already defines
those containerd arguments, edit them there instead of appending duplicates.
When network isolation is enabled, include a snapshot egress policy equivalent
to the one rendered by the Helm example. Render with `kustomize build` before
applying; no cluster change is needed to inspect the result.

## Local checks

```sh
go test ./internal/render -count=1
go test ./internal/controllers -run '^TestSnapshotRuntime' -count=1
bash hack/test-chart.sh
```

The script tests execute the generated commands with stub Docker/nerdctl CLIs
and verify argument handling, failures, and result reporting. Controller tests
use an in-memory Kubernetes client. These checks do not exercise a real runtime
or registry; runtime commit/push and restoration remain deployment acceptance
checks. Registry errors such as HTTP 503 during push must be investigated
separately from container runtime selection.
