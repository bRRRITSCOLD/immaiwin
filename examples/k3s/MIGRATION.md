# Porting the sandbox backend to upstream Kubernetes

The sandbox runtime in `internal/sandbox/k3s/` is named `k3s` for the dev path
but talks to the cluster purely through `client-go`. There is nothing k3s-
specific in the code — only in the bundled `examples/k3s/setup.sh` script,
which exists to bring up a single-node dev environment.

This doc lists what changes when targeting a non-k3s cluster (kubeadm, EKS,
GKE, AKS, Rancher RKE2, etc).

## Configuration changes

Set these env vars on the api process:

| Var | Default (k3s) | Notes |
|-----|---------------|-------|
| `SANDBOX_BACKEND` | `docker` | Set to `k3s` (the runtime is k8s-API based; the env name is historical) |
| `SANDBOX_KUBECONFIG` | `/etc/rancher/k3s/k3s.yaml` | Set to your cluster's kubeconfig, or unset to use in-cluster service-account auth when the api runs in-cluster |
| `SANDBOX_K3S_NAMESPACE` | `immaiwin-sandbox` | Any namespace the api can `create/get/list/delete` Pods + NetworkPolicies in |
| `SANDBOX_K3S_RUNTIMECLASS` | `gvisor` | Must match a RuntimeClass installed on the target cluster |
| `SANDBOX_IMAGE_REGISTRY` | `localhost:5000` | Cluster-reachable registry (e.g. ECR, GCR, Harbor); use the full host:port |
| `SANDBOX_POD_CIDR` | `10.42.0.0/16` | Your cluster's pod CIDR |
| `SANDBOX_SERVICE_CIDR` | `10.43.0.0/16` | Your cluster's service CIDR |
| `SANDBOX_LINKLOCAL_CIDR` | `169.254.0.0/16` | IMDS / link-local. Keep as-is unless on a different cloud |

Common defaults per distro:

| Distro | Pod CIDR | Service CIDR |
|--------|----------|--------------|
| k3s | `10.42.0.0/16` | `10.43.0.0/16` |
| kubeadm (default) | `10.244.0.0/16` | `10.96.0.0/12` |
| EKS | `<vpc cidr>` (per-cluster) | `172.20.0.0/16` |
| GKE | `<cluster cidr>` (per-cluster) | `<cluster service cidr>` |

Get the actual values from:
```bash
kubectl cluster-info dump | grep -E '(pod-network-cidr|service-cluster-ip-range)'
# or:
kubectl get cm -n kube-system kubeadm-config -o yaml | grep -A2 networking
```

## Per-node provisioning

`examples/k3s/setup.sh` patches a single k3s node. On multi-node clusters each
worker node needs:

1. **`runsc` binary** at `/usr/local/bin/runsc`. Options:
   - DaemonSet that ships the binary into a hostPath (e.g. [gvisor's
     `runsc-installer.yaml`](https://gvisor.dev/docs/user_guide/install/))
   - Bake into a custom node image (AMI/disk image)
   - Cloud-init / Ansible per-node setup
2. **containerd config** with the `runsc` runtime handler block. Path differs:
   - kubeadm: `/etc/containerd/config.toml`
   - k3s: `/var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl`
   - RKE2: `/var/lib/rancher/rke2/agent/etc/containerd/config.toml.tmpl`

   Containerd 2.x: use `[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]`.
   Containerd 1.x: use `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]`.

3. **Restart containerd / kubelet** after patch.

## Cluster-level prerequisites

These are applied **once** per cluster, not per node:

- **`RuntimeClass: gvisor`** — already applied by `k3s.New()` on startup; works
  on any k8s.
- **`Namespace: immaiwin-sandbox`** + **NetworkPolicies** (`sandbox-deny-all`,
  `sandbox-egress-only`) — also applied by `k3s.New()`.
- **CNI that enforces NetworkPolicy** — required for security. k3s ships
  kube-router by default. kubeadm needs a CNI install (Calico, Cilium, etc).
  Verify with:
  ```bash
  kubectl get pods -n kube-system | grep -E 'calico|cilium|kube-router|weave'
  ```
- **Image registry the cluster can pull from** — `localhost:5000` only works
  on single-node. Production needs cluster-reachable registry. Configure
  containerd `registries.yaml` (k3s) or `registry-mirror` config (containerd)
  if the registry is insecure (HTTP) or self-signed.

## What stays the same

- Go code in `internal/sandbox/k3s/` — no k3s-only API calls
- Pod spec (RuntimeClass, RunAsUser:10001, drop ALL caps, ActiveDeadlineSeconds)
- NetworkPolicy spec (deny-all + egress-only with configurable Except CIDRs)
- SPDY attach (Run/StreamRun) and SPDY portforward (Debug)
- gVisor portforward bug (#3811) → debug pods stay under runc on any k8s

## Production-only follow-ups

These aren't blockers, but you'll want them for non-dev use:

1. **TLS + auth on the registry** (currently plaintext loopback)
2. **Pod-level resource quotas** in the sandbox namespace (ResourceQuota object)
3. **PriorityClass** for sandbox pods so they evict before workload pods
   under node pressure
4. **Pre-warmed pool** — Deployment with min replicas serving as a sandbox
   queue (cuts cold-start from ~1.5–3s to ~50–100ms)
5. **Multi-replica api support** — current `CleanupOrphans` deletes all
   sandbox-labeled pods at startup, which races with other api replicas.
   Add a per-instance label (`immaiwin.sandbox/instance=<uuid>`) to the
   selector to scope cleanup
6. **Audit logging** — record who ran what code, when. Currently sandbox
   pods are anonymous

When you're ready to deploy, create `examples/k8s/<distro>/` with a
distro-specific Helm chart or kustomize overlay that sets the env vars
above and wires up node provisioning. Don't try to make `examples/k8s/`
generic — every distro has different ergonomics.
