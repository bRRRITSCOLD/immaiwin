package k3s

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Options configures the k3s runtime backend.
type Options struct {
	Kubeconfig     string // path to kubeconfig (defaults to /etc/rancher/k3s/k3s.yaml)
	Namespace      string // sandbox namespace; created if missing
	RuntimeClass   string // PodSpec.RuntimeClassName (e.g. "gvisor")
	Registry       string // image registry prefix (e.g. "localhost:5000")
	DebugPortStart int
	DebugPortEnd   int
	CIDRs          CIDRs // cluster CIDRs blocked from sandbox egress
}

// Runtime executes user code in pods on a k3s cluster with gVisor isolation.
// Implements sandbox.Runtime.
type Runtime struct {
	clientset    kubernetes.Interface
	restConfig   *rest.Config
	ns           string
	runtimeClass string
	registry     string

	bldr          *builder // optional package builder (nil if Docker daemon unavailable)
	portAlloc     *sandbox.PortAllocator
	debugMu       sync.Mutex
	debugSessions map[string]*debugSessionEntry
}

// debugSessionEntry holds runtime state for an active debug session.
// Includes the port-forward stop channel so StopDebug can tear it down.
type debugSessionEntry struct {
	session *sandbox.DebugSession
	stopPF  chan struct{}
}

// New constructs a k3s sandbox runtime. Loads kubeconfig, ensures the
// namespace, applies the gVisor RuntimeClass, and installs default network
// policies (deny-all + egress-only). All idempotent.
func New(opts Options) (*Runtime, error) {
	if opts.Namespace == "" {
		opts.Namespace = "immaiwin-sandbox"
	}
	if opts.RuntimeClass == "" {
		opts.RuntimeClass = "gvisor"
	}
	if opts.Registry == "" {
		return nil, fmt.Errorf("sandbox/k3s: image registry required (set IMAGE_REGISTRY)")
	}

	cfg, err := loadKubeconfig(opts.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox/k3s: kubeconfig: %w", err)
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("sandbox/k3s: clientset: %w", err)
	}

	r := &Runtime{
		clientset:     cs,
		restConfig:    cfg,
		ns:            opts.Namespace,
		runtimeClass:  opts.RuntimeClass,
		registry:      opts.Registry,
		bldr:          newBuilder(opts.Registry),
		debugSessions: make(map[string]*debugSessionEntry),
	}
	if opts.DebugPortStart > 0 && opts.DebugPortEnd > opts.DebugPortStart {
		r.portAlloc = sandbox.NewPortAllocator(opts.DebugPortStart, opts.DebugPortEnd)
	}

	bootCtx := context.Background()

	if err := ensureNamespace(bootCtx, cs, r.ns); err != nil {
		return nil, fmt.Errorf("sandbox/k3s: namespace: %w", err)
	}
	if err := ensureRuntimeClass(bootCtx, cs, r.runtimeClass); err != nil {
		slog.Warn("sandbox/k3s: ensure RuntimeClass failed (continuing)", "err", err)
	}
	if err := ensureNetworkPolicies(bootCtx, cs, r.ns, opts.CIDRs); err != nil {
		slog.Warn("sandbox/k3s: ensure NetworkPolicies failed (continuing)", "err", err)
	}

	slog.Info("sandbox/k3s: runtime ready",
		"namespace", r.ns,
		"runtime_class", r.runtimeClass,
		"registry", r.registry,
	)
	return r, nil
}

// Close releases cluster resources held by the runtime. The k8s clientset has
// no dedicated shutdown — return nil.
func (r *Runtime) Close() error {
	return nil
}

// compile-time assertion: *Runtime implements sandbox.Runtime
var _ sandbox.Runtime = (*Runtime)(nil)

// loadKubeconfig resolves the kubeconfig path or falls back to in-cluster config.
func loadKubeconfig(path string) (*rest.Config, error) {
	if path == "" {
		path = "/etc/rancher/k3s/k3s.yaml"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err == nil {
		return cfg, nil
	}
	// Fall back to in-cluster config if file unreadable
	return rest.InClusterConfig()
}
