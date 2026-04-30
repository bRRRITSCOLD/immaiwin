# k3s + gVisor sandbox backend

Linux-only. Runs untrusted user scripts in k3s pods isolated by gVisor (`runsc`).
Default everywhere else (and on Linux unless explicitly opted-in) is the Docker backend.

## Prerequisites

- Linux host (kernel >= 5.x, x86_64 or arm64)
- Docker daemon (used to build sandbox images and push to local registry)
- `apt`-based distro for the bundled installer (Ubuntu/Debian); other distros: install `runsc` and `k3s` manually following upstream docs.

## One-shot setup

```bash
sudo ./examples/k3s/setup.sh
```

The script installs `runsc` (gVisor), installs `k3s` with NetworkPolicy
enforcement enabled (do **not** pass `--disable-network-policy`), patches the
containerd config template to register the `runsc` runtime, configures k3s to
trust `localhost:5000` as an insecure registry, and runs a `registry:2`
container on port 5000.

## Build + push sandbox images

```bash
make sandbox-images-push        # builds + pushes all 5 lang images (and 2 debug images)
# or override registry:
make sandbox-images-push REGISTRY=registry.local:5000
```

The Makefile builds five base images (python, javascript, golang, rust, php)
plus two debug images (javascript, python) and pushes each tagged
`<REGISTRY>/burrow/sandbox-<lang>:latest`. k3s pulls them on first reference.

## Configure the api process

Set environment variables (see `internal/config/config.go`):

```bash
export SANDBOX_ENABLED=true
export SANDBOX_BACKEND=k3s
export SANDBOX_KUBECONFIG=/etc/rancher/k3s/k3s.yaml
export SANDBOX_K3S_NAMESPACE=burrow-sandbox
export SANDBOX_K3S_RUNTIMECLASS=gvisor
export SANDBOX_IMAGE_REGISTRY=localhost:5000
```

On startup the api process creates the namespace, applies the gVisor
RuntimeClass, and installs default NetworkPolicies (deny-all + egress-only)
in the sandbox namespace. All idempotent.

## Network posture

Two NetworkPolicies select pods by label:

- `burrow.sandbox/network=allow` — egress to public internet allowed; egress
  to k3s pod CIDR `10.42.0.0/16`, service CIDR `10.43.0.0/16`, and link-local
  `169.254.0.0/16` blocked. Ingress denied.
- `burrow.sandbox/network=deny` — ingress + egress denied entirely.

The pod label is set per-run from `RunRequest.Network` (true → allow, false →
deny). No Service exposes sandbox pods, so even without NetworkPolicies there
is no inbound path.

## Verifying the install

```bash
kubectl get nodes
kubectl get runtimeclass            # 'gvisor' should be listed
kubectl -n kube-system get pods | grep kube-router   # NP enforcer
curl -s http://localhost:5000/v2/_catalog            # registry
```

Run a Python script against k3s:

```bash
SANDBOX_BACKEND=k3s SANDBOX_ENABLED=true ./bin/api &
# WS to /api/v1/sandbox/run with {"language":"python","code":"output(input + 1)","input":41}
kubectl -n burrow-sandbox get pods -w     # observe pod scheduling + completion
sudo runsc list                             # populated mid-run
```

## Falling back to Docker on Linux

```bash
SANDBOX_BACKEND=docker SANDBOX_ENABLED=true ./bin/api
```

The Docker backend uses the same Dockerfiles + entrypoints; nothing changes
except that pods are containers and gVisor is configured at the Docker daemon
level if `SANDBOX_RUNTIME=runsc`.

## Caveats

- **Single replica only.** Orphan pod cleanup at startup uses a label selector
  scoped to the namespace; two api replicas would race-delete each other's
  in-flight pods.
- **Insecure registry on loopback.** `localhost:5000` is plaintext; acceptable
  on the same host but production deployments should front the registry with
  TLS + auth.
- **Docker daemon still required for package builds.** The api process uses
  the local Docker daemon to build package-augmented images, then pushes them
  to the registry. A pure Docker-less k3s host is not yet supported.
- **Cold-start latency.** Pod schedule + containerd unpack + gVisor init runs
  ~1.5–3 s. A pre-warmed Deployment is a follow-up; for now every run pays
  cold-start cost.
