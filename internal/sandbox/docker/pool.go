// Package docker — warm container pool, keyed by (language, image-tag).
//
// Pre-creates a small set of containers per (language, image-tag)
// at startup (defaults) and on-demand (custom-image / packages).
// On Acquire, a container is plucked from the pool and the
// existing Run path attaches to it instead of paying the create +
// image-pull overhead. After use the container is removed
// (single-use under stdinOnce) and a fresh one is created in the
// background to replenish the slot.
//
// Pool key includes the resolved image tag so package-using runs
// (which use docker BuildOrReuse for a deterministic tag) hit a
// per-package warm pool from the second run onwards. First run
// still pays cold-start; subsequent runs with the SAME packages
// list hit the pool. Total unique keys capped at maxKeys (default
// 16), FIFO-evict on overflow.

package docker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// poolKey identifies a unique warm-container template. Same key
// = interchangeable containers (same lang, image, network mode,
// resource sizing). Resource + network fields are in the key
// because container HostConfig bakes them at create time.
type poolKey struct {
	lang      sandbox.Language
	imageTag  string
	network   bool
	memBytes  int64
	cpuMillis int64
}

// Pool maintains warm containers per (lang, image-tag) key.
type Pool struct {
	mu       sync.Mutex
	cli      client.APIClient
	warm     map[poolKey][]string
	keyOrder []poolKey
	maxWarm  int
	maxKeys  int
	runtime  string
	closed   bool
}

// NewPool creates a container pool. maxPerKey = warm containers
// per (lang, image-tag).
func NewPool(cli client.APIClient, maxPerKey int, runtime string) *Pool {
	if maxPerKey <= 0 {
		maxPerKey = 2
	}
	return &Pool{
		cli:     cli,
		warm:    make(map[poolKey][]string),
		maxWarm: maxPerKey,
		maxKeys: 16,
		runtime: runtime,
	}
}

// Warm pre-creates idle containers for the default base image of
// each language. Custom-image / packages keys warm lazily on
// first Acquire miss.
func (p *Pool) Warm(ctx context.Context, langs ...sandbox.Language) {
	for _, lang := range langs {
		key := poolKey{
			lang:      lang,
			imageTag:  sandbox.ImageForLanguage(lang),
			network:   false,
			memBytes:  sandbox.DefaultMemLimit,
			cpuMillis: int64(sandbox.DefaultCPULimit * 1000),
		}
		if key.imageTag == "" {
			continue
		}
		for i := 0; i < p.maxWarm; i++ {
			id, err := p.createWarm(ctx, key)
			if err != nil {
				slog.Warn("sandbox/docker pool: warm failed", "lang", lang, "image", key.imageTag, "err", err)
				continue
			}
			p.addToKey(key, id)
		}
	}
}

// Acquire returns a warm container ID matching the request's
// envelope, or empty if no slot ready. Always triggers replenish.
func (p *Pool) Acquire(req sandbox.RunRequest, imageTag string) string {
	key := poolKey{
		lang:      req.Language,
		imageTag:  imageTag,
		network:   req.Network,
		memBytes:  req.MemLimit,
		cpuMillis: int64(req.CPULimit * 1000),
	}
	p.mu.Lock()
	ids := p.warm[key]
	if len(ids) == 0 {
		p.mu.Unlock()
		// Cold miss — replenish so the SECOND request for this
		// key hits warm.
		go p.replenish(key)
		return ""
	}
	id := ids[0]
	p.warm[key] = ids[1:]
	p.touchKey(key)
	p.mu.Unlock()

	go p.replenish(key)
	return id
}

// Release removes a used container (single-use under stdinOnce).
func (p *Pool) Release(ctx context.Context, containerID string) {
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = p.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
}

// Close drains and removes all warm containers.
func (p *Pool) Close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	all := make([]string, 0)
	for _, ids := range p.warm {
		all = append(all, ids...)
	}
	p.warm = make(map[poolKey][]string)
	p.keyOrder = nil
	p.mu.Unlock()

	for _, id := range all {
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = p.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
		cancel()
	}
}

func (p *Pool) replenish(key poolKey) {
	p.mu.Lock()
	if p.closed || len(p.warm[key]) >= p.maxWarm {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := p.createWarm(ctx, key)
	if err != nil {
		slog.Warn("sandbox/docker pool: replenish failed", "lang", key.lang, "image", key.imageTag, "err", err)
		return
	}
	if !p.addToKey(key, id) {
		go func() {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rmCancel()
			_ = p.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
		}()
	}
}

func (p *Pool) addToKey(key poolKey, id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.warm[key]) >= p.maxWarm {
		return false
	}
	if _, exists := p.warm[key]; !exists {
		if len(p.keyOrder) >= p.maxKeys {
			oldest := p.keyOrder[0]
			p.keyOrder = p.keyOrder[1:]
			victims := p.warm[oldest]
			delete(p.warm, oldest)
			go func(ids []string) {
				for _, cid := range ids {
					rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = p.cli.ContainerRemove(rmCtx, cid, container.RemoveOptions{Force: true})
					rmCancel()
				}
			}(victims)
		}
		p.keyOrder = append(p.keyOrder, key)
	}
	p.warm[key] = append(p.warm[key], id)
	return true
}

func (p *Pool) touchKey(key poolKey) {
	for i, k := range p.keyOrder {
		if k == key {
			p.keyOrder = append(p.keyOrder[:i], p.keyOrder[i+1:]...)
			p.keyOrder = append(p.keyOrder, key)
			return
		}
	}
}

func (p *Pool) createWarm(ctx context.Context, key poolKey) (string, error) {
	if key.imageTag == "" {
		return "", fmt.Errorf("sandbox/docker pool: empty image tag for lang %s", key.lang)
	}
	memBytes := key.memBytes
	if memBytes == 0 {
		memBytes = sandbox.DefaultMemLimit
	}
	cpuMillis := key.cpuMillis
	if cpuMillis == 0 {
		cpuMillis = int64(sandbox.DefaultCPULimit * 1000)
	}
	nanoCPUs := cpuMillis * 1_000_000 // millicores → nanocores

	cfg := &container.Config{
		Image:       key.imageTag,
		Labels:      map[string]string{sandbox.SandboxLabel: "true"},
		AttachStdin: true,
		OpenStdin:   true,
		StdinOnce:   true,
		Tty:         false,
	}

	netMode := container.NetworkMode("none")
	if key.network {
		netMode = "bridge"
	}
	hostCfg := &container.HostConfig{
		Resources: container.Resources{
			Memory:    memBytes,
			NanoCPUs:  nanoCPUs,
			PidsLimit: intPtr(256),
		},
		NetworkMode: netMode,
	}
	if p.runtime != "" {
		hostCfg.Runtime = p.runtime
	}

	resp, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("create warm container: %w", err)
	}
	return resp.ID, nil
}
