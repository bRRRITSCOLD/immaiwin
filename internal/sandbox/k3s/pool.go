// Package k3s — warm pod pool, keyed by (language, image-tag).
//
// Pre-creates a small set of Pods per (language, image-tag) at
// startup (defaults) and on-demand (custom-image / packages),
// blocked on stdin (Pod's user container is `stdin: true,
// stdinOnce: true`), waiting for the first attach. On Acquire, a
// pod is plucked from the pool and the existing Run path attaches
// to it instead of paying the create + schedule + image-pull
// overhead per request. After use the pod is deleted (Pods are
// one-shot under stdinOnce) and a fresh one is created in the
// background to replenish the slot.
//
// Pool key includes the resolved image tag so package-using runs
// (which build a derived image once + push to registry) hit a
// per-package warm pool from the second run onwards. First run
// still pays cold-start (per-run pod create); subsequent runs
// with the SAME packages list hit the pool. The pool caps total
// unique keys at maxKeys (default 16) and FIFO-evicts the oldest
// key when at cap.
//
// Eligibility (defaults required): mem == DefaultMemLimit, cpu
// == DefaultCPULimit, network == false. Custom resource requests
// fall through to per-run create — pool pods are sized at create
// and can't satisfy arbitrary later limits.

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
// created pod to enter Running. Long enough for an image pull on
// a cold registry, short enough that a stuck node doesn't block
// the pool indefinitely.
const poolPodTimeout = 90 * time.Second

// warmPodIdleTTL is the activeDeadlineSeconds stamped on warm pods.
// Pool pods sit idle until an Acquire picks them up — must outlive
// the default 30s execution timeout that buildPod inherits from
// req.Timeout. 24h is well over any realistic Acquire latency
// while still bounding orphan pods if the controlling worker
// exits without Close().
const warmPodIdleTTL = 24 * time.Hour

// poolKey identifies a unique warm-pod template. Two requests
// hash to the same key (and therefore share warm pods) iff they
// produce an interchangeable pod — same lang, same resolved
// image, same network policy, same resource sizing.
//
// Resource fields are required in the key because pod
// spec.containers[].resources.{limits,requests} + the network
// policy label are baked at create time and cannot be changed
// to satisfy a later request with a different envelope.
type poolKey struct {
	lang      sandbox.Language
	imageTag  string
	network   bool
	memBytes  int64
	cpuMillis int64
}

// Pool maintains warm pods per (lang, image-tag) key. Acquire
// pulls one for a matching key; Release deletes the used pod
// (single-use under stdinOnce). Replenish runs asynchronously to
// keep each touched key topped up.
type Pool struct {
	mu       sync.Mutex
	cli      kubernetes.Interface
	rt       *Runtime
	warm     map[poolKey][]string
	keyOrder []poolKey // FIFO eviction order — oldest-touched first
	maxWarm  int
	maxKeys  int
	closed   bool
}

// NewPool creates a pod pool. maxPerKey controls how many warm
// pods are kept per (lang, image-tag) (default 2).
func NewPool(rt *Runtime, maxPerKey int) *Pool {
	if maxPerKey <= 0 {
		maxPerKey = 2
	}
	return &Pool{
		cli:     rt.clientset,
		rt:      rt,
		warm:    make(map[poolKey][]string),
		maxWarm: maxPerKey,
		maxKeys: 16,
	}
}

// Warm pre-creates idle pods for the default base image of each
// language at default resources + network=false. Custom keys
// (different image / network / mem / cpu) warm lazily on first
// Acquire miss.
func (p *Pool) Warm(ctx context.Context, langs ...sandbox.Language) {
	for _, lang := range langs {
		key := poolKey{
			lang:      lang,
			imageTag:  p.rt.imageWithRegistry(sandbox.ImageForLanguage(lang)),
			network:   false,
			memBytes:  sandbox.DefaultMemLimit,
			cpuMillis: int64(sandbox.DefaultCPULimit * 1000),
		}
		for i := 0; i < p.maxWarm; i++ {
			name, err := p.createWarm(ctx, key)
			if err != nil {
				slog.Warn("sandbox/k3s pool: warm failed", "lang", lang, "image", key.imageTag, "err", err)
				continue
			}
			p.addToKey(key, name)
		}
	}
}

// Acquire returns a warm pod name matching the request's
// envelope, or empty string if none available. Always triggers
// replenish so the next request for the same key hits the pool.
// Caller MUST treat the pod as single-use: attach, run payload,
// delete.
func (p *Pool) Acquire(req sandbox.RunRequest, imageTag string) string {
	key := poolKey{
		lang:      req.Language,
		imageTag:  imageTag,
		network:   req.Network,
		memBytes:  req.MemLimit,
		cpuMillis: int64(req.CPULimit * 1000),
	}
	for {
		p.mu.Lock()
		ids := p.warm[key]
		if len(ids) == 0 {
			p.mu.Unlock()
			// Cold miss: trigger a replenish so a SECOND request
			// for this key lands on a warm pod instead of paying
			// cold-start twice in a row. The current caller still
			// falls through to per-run create.
			go p.replenish(key)
			return ""
		}
		name := ids[0]
		p.warm[key] = ids[1:]
		p.touchKey(key) // mark recent so eviction prefers cold keys
		p.mu.Unlock()

		// Liveness check — quick GET. K8s can kill an idle pod
		// (OOM / eviction / node restart) and the pool's in-memory
		// slot would silently lie. Returning a dead pod fails
		// attach with "pod entered terminal phase Failed".
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pod, err := p.cli.CoreV1().Pods(p.rt.ns).Get(checkCtx, name, metav1.GetOptions{})
		cancel()
		if err == nil && pod.Status.Phase == "Running" {
			go p.replenish(key)
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
		go p.replenish(key)
		// loop tries the next slot
	}
}

// Release deletes a used pod. Same as Docker pool — stdinOnce
// means each pod is single-use.
func (p *Pool) Release(ctx context.Context, name string) {
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deletePod(rmCtx, p.cli, p.rt.ns, name)
}

// Close drains and deletes every warm pod across every key.
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

	for _, name := range all {
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		deletePod(rmCtx, p.cli, p.rt.ns, name)
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

	ctx, cancel := context.WithTimeout(context.Background(), poolPodTimeout+30*time.Second)
	defer cancel()

	name, err := p.createWarm(ctx, key)
	if err != nil {
		slog.Warn("sandbox/k3s pool: replenish failed", "lang", key.lang, "image", key.imageTag, "err", err)
		return
	}
	if !p.addToKey(key, name) {
		// Slot full / closed during the wait — drop the extra pod.
		go func(podName string) {
			rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer rmCancel()
			deletePod(rmCtx, p.cli, p.rt.ns, podName)
		}(name)
	}
}

// addToKey appends a warm pod to the key's slot and tracks key
// order for eviction. Returns false (caller deletes the pod) if
// the pool is closed or the slot is already full.
func (p *Pool) addToKey(key poolKey, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		slog.Info("sandbox/k3s pool: addToKey skipped — closed", "pod", name, "lang", key.lang)
		return false
	}
	if len(p.warm[key]) >= p.maxWarm {
		slog.Info("sandbox/k3s pool: addToKey skipped — slot full", "pod", name, "lang", key.lang, "image", key.imageTag, "slot_len", len(p.warm[key]))
		return false
	}
	// New key — enforce maxKeys cap via FIFO eviction of the
	// oldest-touched key. Drop its warm pods in the background.
	if _, exists := p.warm[key]; !exists {
		if len(p.keyOrder) >= p.maxKeys {
			oldest := p.keyOrder[0]
			p.keyOrder = p.keyOrder[1:]
			victims := p.warm[oldest]
			delete(p.warm, oldest)
			go func(names []string) {
				for _, podName := range names {
					rmCtx, rmCancel := context.WithTimeout(context.Background(), 10*time.Second)
					deletePod(rmCtx, p.cli, p.rt.ns, podName)
					rmCancel()
				}
			}(victims)
		}
		p.keyOrder = append(p.keyOrder, key)
	}
	p.warm[key] = append(p.warm[key], name)
	slog.Info("sandbox/k3s pool: warm slot updated", "pod", name, "lang", key.lang, "image", key.imageTag, "slot_len", len(p.warm[key]), "keys", len(p.warm))
	return true
}

// touchKey moves a key to the back of keyOrder so the next FIFO
// eviction targets the least-recently-touched key instead of
// this one. Caller must hold p.mu.
func (p *Pool) touchKey(key poolKey) {
	for i, k := range p.keyOrder {
		if k == key {
			p.keyOrder = append(p.keyOrder[:i], p.keyOrder[i+1:]...)
			p.keyOrder = append(p.keyOrder, key)
			return
		}
	}
}

// createWarm spins up a warm pod for the given key. Default
// resources, network=none, long deadline. Waits for Running
// before returning so Acquire callers attach immediately.
func (p *Pool) createWarm(ctx context.Context, key poolKey) (string, error) {
	memBytes := key.memBytes
	if memBytes == 0 {
		memBytes = sandbox.DefaultMemLimit
	}
	cpuMillis := key.cpuMillis
	if cpuMillis == 0 {
		cpuMillis = int64(sandbox.DefaultCPULimit * 1000)
	}
	req := sandbox.RunRequest{
		Language: key.lang,
		MemLimit: memBytes,
		CPULimit: float64(cpuMillis) / 1000,
		Timeout:  warmPodIdleTTL,
		Network:  key.network,
	}
	pod := p.rt.buildPod(req, key.imageTag, false)
	created, err := p.cli.CoreV1().Pods(p.rt.ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		slog.Warn("sandbox/k3s pool: create pod failed", "lang", key.lang, "image", key.imageTag, "err", err)
		return "", err
	}
	slog.Info("sandbox/k3s pool: pod creating", "pod", created.Name, "lang", key.lang, "image", key.imageTag)
	if werr := waitPodRunning(ctx, p.cli, p.rt.ns, created.Name, poolPodTimeout); werr != nil {
		slog.Warn("sandbox/k3s pool: pod never reached Running", "pod", created.Name, "lang", key.lang, "image", key.imageTag, "err", werr)
		deletePod(context.Background(), p.cli, p.rt.ns, created.Name)
		return "", werr
	}
	slog.Info("sandbox/k3s pool: warm pod ready", "pod", created.Name, "lang", key.lang, "image", key.imageTag)
	return created.Name, nil
}
