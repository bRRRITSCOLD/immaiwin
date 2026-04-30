// Sandbox runtime builder for worker processes. Mirrors cmd/api so
// cron-driven and event-driven workflows execute skill tools (which
// require a sandbox) the same way as canvas-driven workflows. Failures
// to construct the sandbox return nil + log a warning rather than
// failing the worker — workflows without skill / sandbox-script nodes
// keep working without a runtime.

package worker

import (
	"context"
	"log/slog"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/bRRRITSCOLD/burrow/internal/sandbox/docker"
	"github.com/bRRRITSCOLD/burrow/internal/sandbox/k3s"
)

// buildSandboxRT constructs a sandbox runtime per the config. Backend
// selection mirrors cmd/api: docker by default, k3s opt-in.
func buildSandboxRT(ctx context.Context, cfg *config.Config) sandbox.Runtime {
	if !cfg.Sandbox.Enabled {
		return nil
	}
	backend := cfg.Sandbox.Backend
	if backend == "" || backend == "auto" {
		backend = "docker"
	}
	switch backend {
	case "docker":
		rt, err := docker.New(docker.Options{
			Runtime:        cfg.Sandbox.Runtime,
			DebugPortStart: 19000,
			DebugPortEnd:   19100,
			Registry:       cfg.Sandbox.ImageRegistry,
		})
		if err != nil {
			slog.Warn("worker: docker sandbox init failed (skill tools will be unavailable)", "err", err)
			return nil
		}
		rt.CleanupOrphans(ctx)
		pool := docker.NewPool(rt.DockerClient(), cfg.Sandbox.PoolSize, cfg.Sandbox.Runtime)
		pool.Warm(ctx, sandbox.LangJavaScript, sandbox.LangPython)
		rt.SetPool(pool)
		slog.Info("worker: sandbox enabled", "backend", "docker", "runtime", cfg.Sandbox.Runtime, "pool_size", cfg.Sandbox.PoolSize)
		return rt
	case "k3s":
		rt, err := k3s.New(k3s.Options{
			Kubeconfig:     cfg.Sandbox.Kubeconfig,
			Namespace:      cfg.Sandbox.Namespace,
			RuntimeClass:   cfg.Sandbox.RuntimeClass,
			Registry:       cfg.Sandbox.ImageRegistry,
			DebugPortStart: 19000,
			DebugPortEnd:   19100,
			CIDRs: k3s.CIDRs{
				Pod:       cfg.Sandbox.PodCIDR,
				Service:   cfg.Sandbox.ServiceCIDR,
				LinkLocal: cfg.Sandbox.LinkLocalCIDR,
			},
		})
		if err != nil {
			slog.Warn("worker: k3s sandbox init failed (skill tools will be unavailable)", "err", err)
			return nil
		}
		rt.CleanupOrphans(ctx)
		slog.Info("worker: sandbox enabled", "backend", "k3s", "namespace", cfg.Sandbox.Namespace, "registry", cfg.Sandbox.ImageRegistry)
		return rt
	default:
		slog.Warn("worker: unknown sandbox backend (skill tools will be unavailable)", "backend", backend)
		return nil
	}
}
