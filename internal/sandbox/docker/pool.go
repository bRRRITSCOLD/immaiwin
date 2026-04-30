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

// Pool maintains a set of warm, idle containers per language for fast starts.
// When a container is acquired, a new one is created in the background to
// replenish the pool.
type Pool struct {
	mu      sync.Mutex
	cli     client.APIClient
	warm    map[sandbox.Language][]string
	maxWarm int
	runtime string
	closed  bool
}

// NewPool creates a container pool. maxPerLang controls how many warm
// containers are kept per language (default 2).
func NewPool(cli client.APIClient, maxPerLang int, runtime string) *Pool {
	if maxPerLang <= 0 {
		maxPerLang = 2
	}
	return &Pool{
		cli:     cli,
		warm:    make(map[sandbox.Language][]string),
		maxWarm: maxPerLang,
		runtime: runtime,
	}
}

// Warm pre-creates idle containers for the given languages.
// Call once at startup.
func (p *Pool) Warm(ctx context.Context, langs ...sandbox.Language) {
	for _, lang := range langs {
		for i := 0; i < p.maxWarm; i++ {
			id, err := p.createWarm(ctx, lang)
			if err != nil {
				slog.Warn("sandbox/docker pool: warm failed", "lang", lang, "err", err)
				continue
			}
			p.mu.Lock()
			p.warm[lang] = append(p.warm[lang], id)
			p.mu.Unlock()
		}
	}
}

// Acquire returns a warm container ID for the language, or empty string if none available.
func (p *Pool) Acquire(lang sandbox.Language) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	ids := p.warm[lang]
	if len(ids) == 0 {
		return ""
	}

	id := ids[0]
	p.warm[lang] = ids[1:]

	go p.replenish(lang)

	return id
}

// Release removes a used container (containers are single-use after exec).
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
	p.warm = make(map[sandbox.Language][]string)
	p.mu.Unlock()

	for _, id := range all {
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = p.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
		cancel()
	}
}

func (p *Pool) replenish(lang sandbox.Language) {
	p.mu.Lock()
	if p.closed || len(p.warm[lang]) >= p.maxWarm {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id, err := p.createWarm(ctx, lang)
	if err != nil {
		slog.Warn("sandbox/docker pool: replenish failed", "lang", lang, "err", err)
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && len(p.warm[lang]) < p.maxWarm {
		p.warm[lang] = append(p.warm[lang], id)
	} else {
		go func() {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rmCancel()
			_ = p.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
		}()
	}
}

func (p *Pool) createWarm(ctx context.Context, lang sandbox.Language) (string, error) {
	img := sandbox.ImageForLanguage(lang)
	if img == "" {
		return "", fmt.Errorf("unsupported language: %s", lang)
	}

	cfg := &container.Config{
		Image:       img,
		Labels:      map[string]string{sandbox.SandboxLabel: "true"},
		AttachStdin: true,
		OpenStdin:   true,
		StdinOnce:   true,
		Tty:         false,
	}

	hostCfg := &container.HostConfig{
		Resources: container.Resources{
			Memory:    sandbox.DefaultMemLimit,
			NanoCPUs:  int64(sandbox.DefaultCPULimit * 1e9),
			PidsLimit: intPtr(256),
		},
		NetworkMode: "none",
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
