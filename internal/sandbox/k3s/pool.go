// Package k3s — warm pod pool.
//
// Mirrors the Docker-side `pool.go` shape: pre-creates a small set of
// Pods per language at startup, blocked on stdin (Pod's user
// container is `stdin: true, stdinOnce: true`), waiting for the first
// attach. On Acquire, a pod is plucked from the pool and the existing
// Run path attaches to it instead of paying the create + schedule +
// image-pull overhead per request. After use the pod is deleted
// (Pods are one-shot under stdinOnce) and a fresh one is created in
// the background to replenish the pool.
//
// Eligibility is restrictive on purpose: pool pods are sized with
// the default mem/cpu, run with `network=none`, and use the registry-
// prefixed base image for the language. Any request whose RunRequest
// diverges (custom image, packages, network=true, non-default
// resources) skips the pool and falls through to the per-run create
// path. Loosening eligibility (per-(image,resources) pools, package-
// image pools) is a follow-up — this PR delivers the most-common-
// case win: default Python / JS / Go / Rust / PHP runs avoid pod
// scheduling cold start on every dispatch.

package k3s

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// poolPodTimeout is how long Warm / Replenish wait for a newly-
// created pod to enter Running. Long enough for an image pull on a
// cold registry, short enough that a stuck node doesn't block the
// pool indefinitely.
const poolPodTimeout = 90 * time.Second

// Pool maintains a set of warm pods per language. Acquire pulls one
// from the pool; Release deletes the used pod (single-use under
// stdinOnce). Replenish runs asynchronously to keep the pool topped
// up.
type Pool struct {
	mu      sync.Mutex
	cli     kubernetes.Interface
	rt      *Runtime // for buildPod + resolveImage + namespace
	warm    map[sandbox.Language][]string
	maxWarm int
	closed  bool
}

// NewPool creates a pod pool. maxPerLang controls how many warm
// pods are kept per language (default 2).
func NewPool(rt *Runtime, maxPerLang int) *Pool {
	if maxPerLang <= 0 {
		maxPerLang = 2
	}
	return &Pool{
		cli:     rt.clientset,
		rt:      rt,
		warm:    make(map[sandbox.Language][]string),
		maxWarm: maxPerLang,
	}
}

// Warm pre-creates idle pods for the given languages. Each pod
// blocks at stdin EOF inside its container's entrypoint until
// attach delivers the actual payload. Call once at startup.
func (p *Pool) Warm(ctx context.Context, langs ...sandbox.Language) {
	for _, lang := range langs {
		for i := 0; i < p.maxWarm; i++ {
			name, err := p.createWarm(ctx, lang)
			if err != nil {
				slog.Warn("sandbox/k3s pool: warm failed", "lang", lang, "err", err)
				continue
			}
			p.mu.Lock()
			p.warm[lang] = append(p.warm[lang], name)
			p.mu.Unlock()
		}
	}
}

// Acquire returns a warm pod name for the language, or empty
// string if none available. Caller MUST treat the pod as
// single-use: attach, run the user payload, delete.
//
// Validates pod phase before returning — k8s can kill a pod for
// many reasons (OOM, node eviction, exhausted activeDeadline)
// while it's idle in the pool. Returning a dead pod would fail
// the next attach with "pod entered terminal phase Failed". The
// validation loop skips dead pods (deletes them in the
// background) and tries the next slot.
func (p *Pool) Acquire(lang sandbox.Language) string {
	for {
		p.mu.Lock()
		ids := p.warm[lang]
		if len(ids) == 0 {
			p.mu.Unlock()
			return ""
		}
		name := ids[0]
		p.warm[lang] = ids[1:]
		p.mu.Unlock()

		// Liveness check — quick GET. If pod is Running, hand it
		// back. If not, dispose + try next.
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pod, err := p.cli.CoreV1().Pods(p.rt.ns).Get(checkCtx, name, metav1.GetOptions{})
		cancel()
		if err == nil && pod.Status.Phase == "Running" {
			go p.replenish(lang)
			return name
		}
		phase := ""
		if pod != nil {
			phase = string(pod.Status.Phase)
		}
		slog.Warn("sandbox/k3s pool: discarding dead warm pod", "pod", name, "phase", phase, "err", err)
		go func(podName string) {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rmCancel()
			deletePod(rmCtx, p.cli, p.rt.ns, podName)
		}(name)
		go p.replenish(lang)
		// loop to try the next slot
	}
}

// Release deletes a used pod. Matching the Docker pool's per-
// request lifecycle: stdinOnce means the pod cannot be reused.
func (p *Pool) Release(ctx context.Context, name string) {
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deletePod(rmCtx, p.cli, p.rt.ns, name)
}

// Close drains and deletes all warm pods.
func (p *Pool) Close(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	all := make([]string, 0)
	for _, ids := range p.warm {
		all = append(all, ids...)
	}
	p.warm = make(map[sandbox.Language][]string)
	p.mu.Unlock()

	for _, name := range all {
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		deletePod(rmCtx, p.cli, p.rt.ns, name)
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

	ctx, cancel := context.WithTimeout(context.Background(), poolPodTimeout+30*time.Second)
	defer cancel()

	name, err := p.createWarm(ctx, lang)
	if err != nil {
		slog.Warn("sandbox/k3s pool: replenish failed", "lang", lang, "err", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && len(p.warm[lang]) < p.maxWarm {
		p.warm[lang] = append(p.warm[lang], name)
	} else {
		// Pool already full / closed during the wait — drop the
		// extra pod so it doesn't leak.
		go func(podName string) {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rmCancel()
			deletePod(rmCtx, p.cli, p.rt.ns, podName)
		}(name)
	}
}

// createWarm spins up a single warm pod for the language: default
// resources, network=none, no custom image, no packages. Waits for
// Running before returning so Acquire callers can attach
// immediately.
// warmPodIdleTTL is the activeDeadlineSeconds stamped on warm pods.
// Pool pods sit idle until an Acquire picks them up — must outlive
// the default 30s execution timeout that buildPod inherits from
// req.Timeout. 24h is well over any realistic Acquire latency
// (pool keeps a handful warm) while still bounding orphan pods if
// the controlling worker exits without Close().
const warmPodIdleTTL = 24 * time.Hour

func (p *Pool) createWarm(ctx context.Context, lang sandbox.Language) (string, error) {
	req := sandbox.RunRequest{
		Language: lang,
		MemLimit: sandbox.DefaultMemLimit,
		CPULimit: sandbox.DefaultCPULimit,
		// Long deadline keeps the pod alive while waiting for an
		// Acquire. Without this, buildPod stamps the default 30s
		// execution timeout into activeDeadlineSeconds; the pod
		// then enters Failed before any attach can land, and pool
		// acquires return a dead pod that immediately errors on
		// attach.
		Timeout: warmPodIdleTTL,
		Network: false,
	}
	image := p.rt.imageWithRegistry(sandbox.ImageForLanguage(lang))
	pod := p.rt.buildPod(req, image, false)
	created, err := p.cli.CoreV1().Pods(p.rt.ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	// Wait for the user container to report Running. If it doesn't
	// inside the timeout, delete + return the error so Warm /
	// Replenish can log + retry rather than leaving an orphan.
	if werr := waitPodRunning(ctx, p.cli, p.rt.ns, created.Name, poolPodTimeout); werr != nil {
		deletePod(context.Background(), p.cli, p.rt.ns, created.Name)
		return "", werr
	}
	return created.Name, nil
}
