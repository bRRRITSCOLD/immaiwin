#!/usr/bin/env bash
# k3s + gVisor + local registry setup for immaiwin sandbox.
# Idempotent — safe to re-run.
set -euo pipefail

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "ERROR: k3s sandbox backend only supported on Linux." >&2
  exit 1
fi

if [[ $EUID -ne 0 ]]; then
  echo "Re-running with sudo..." >&2
  exec sudo -E bash "$0" "$@"
fi

# 1. Install gVisor (runsc) if missing
if ! command -v runsc >/dev/null 2>&1; then
  echo "==> installing gVisor (runsc)..."
  curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
    > /etc/apt/sources.list.d/gvisor.list
  apt-get update
  apt-get install -y runsc
fi
runsc --version

# 2. Install k3s if missing
if ! command -v k3s >/dev/null 2>&1; then
  echo "==> installing k3s..."
  curl -sfL https://get.k3s.io | sh -s - server \
    --write-kubeconfig-mode=644 \
    --disable=traefik \
    --disable=servicelb
fi
k3s --version

# 3. Patch containerd config template for runsc handler.
# k3s ships containerd 2.x which uses the "io.containerd.cri.v1.runtime" plugin
# path (the older "io.containerd.grpc.v1.cri" path is silently ignored).
TPL=/var/lib/rancher/k3s/agent/etc/containerd/config.toml.tmpl
if [[ ! -f "$TPL" ]] || ! grep -q 'cri.v1.runtime".containerd.runtimes.runsc' "$TPL"; then
  echo "==> patching containerd config template..."
  if [[ ! -f "$TPL" ]]; then
    # Bootstrap from current rendered config
    mkdir -p "$(dirname "$TPL")"
    cp /var/lib/rancher/k3s/agent/etc/containerd/config.toml "$TPL"
  fi
  cat <<'EOF' >> "$TPL"

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
EOF
  systemctl restart k3s
fi

# 4. Configure k3s registry mirror for localhost:5000
REG=/etc/rancher/k3s/registries.yaml
if [[ ! -f "$REG" ]] || ! grep -q "localhost:5000" "$REG"; then
  echo "==> writing $REG"
  cat > "$REG" <<'EOF'
mirrors:
  "localhost:5000":
    endpoint:
      - "http://localhost:5000"
configs:
  "localhost:5000":
    tls:
      insecure_skip_verify: true
EOF
  systemctl restart k3s
fi

# 5. Run a local registry (skipped if already present)
if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^registry$'; then
  echo "==> starting local registry on :5000"
  docker run -d --restart=always -p 5000:5000 --name registry registry:2
fi

echo
echo "Setup complete. Verify with:"
echo "  kubectl get nodes"
echo "  kubectl get runtimeclass"
echo "  curl -s http://localhost:5000/v2/_catalog"
