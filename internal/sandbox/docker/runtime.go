package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/sandbox"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Options configures the Docker runtime backend.
type Options struct {
	Runtime        string // OCI runtime override (e.g. "runsc" for gVisor); empty = default
	DebugPortStart int
	DebugPortEnd   int
	Registry       string // optional image registry prefix (e.g. "localhost:5000")
}

// Runtime creates and runs sandbox containers via the Docker daemon.
// Implements sandbox.Runtime.
type Runtime struct {
	cli           client.APIClient
	runtime       string
	registry      string
	pool          *Pool
	builder       *ImageBuilder
	portAlloc     *sandbox.PortAllocator
	debugMu       sync.Mutex
	debugSessions map[string]*sandbox.DebugSession
}

// New creates a Docker-backed sandbox runtime. It connects to the Docker daemon
// using environment defaults (DOCKER_HOST, etc.).
func New(opts Options) (*Runtime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox/docker: client: %w", err)
	}

	r := &Runtime{
		cli:           cli,
		runtime:       opts.Runtime,
		registry:      opts.Registry,
		builder:       NewImageBuilder(cli, opts.Registry),
		debugSessions: make(map[string]*sandbox.DebugSession),
	}
	if opts.DebugPortStart > 0 && opts.DebugPortEnd > opts.DebugPortStart {
		r.portAlloc = sandbox.NewPortAllocator(opts.DebugPortStart, opts.DebugPortEnd)
	}
	return r, nil
}

// DockerClient exposes the underlying Docker client (e.g. for pool creation).
// Backend-specific accessor; not on the sandbox.Runtime interface.
func (r *Runtime) DockerClient() client.APIClient {
	return r.cli
}

// SetPool attaches a warm container pool post-construction.
func (r *Runtime) SetPool(p *Pool) {
	r.pool = p
}

// Close releases the Docker client.
func (r *Runtime) Close() error {
	return r.cli.Close()
}

// compile-time assertion: *Runtime implements sandbox.Runtime
var _ sandbox.Runtime = (*Runtime)(nil)

// resolveImage determines the final image tag for a run.
// Priority: custom_image > packages (auto-build) > default base.
func (r *Runtime) resolveImage(ctx context.Context, lang sandbox.Language, customImage string, packages []string, debug bool) (string, error) {
	if customImage != "" {
		return customImage, nil
	}

	var base string
	if debug {
		base = sandbox.DebugImageForLanguage(lang)
	} else {
		base = sandbox.ImageForLanguage(lang)
	}
	if base == "" {
		return "", fmt.Errorf("sandbox/docker: unsupported language: %s", lang)
	}

	if len(packages) > 0 {
		return r.builder.BuildOrReuse(ctx, lang, base, packages, debug)
	}

	return base, nil
}

// CleanupOrphans removes any sandbox containers left over from a previous
// crashed session. Call once at startup before pool.Warm().
func (r *Runtime) CleanupOrphans(ctx context.Context) {
	f := filters.NewArgs(filters.Arg("label", sandbox.SandboxLabel+"=true"))
	containers, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		slog.Warn("sandbox/docker: orphan cleanup list failed", "err", err)
		return
	}
	if len(containers) == 0 {
		return
	}
	slog.Info("sandbox/docker: cleaning up orphan containers", "count", len(containers))
	for _, c := range containers {
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = r.cli.ContainerRemove(rmCtx, c.ID, container.RemoveOptions{Force: true})
		cancel()
		slog.Info("sandbox/docker: removed orphan", "container", c.ID[:12])
	}
}

// sandboxLabels returns the label map for sandbox containers.
func sandboxLabels() map[string]string {
	return map[string]string{sandbox.SandboxLabel: "true"}
}

// Run executes user code in an isolated container and returns the result.
func (r *Runtime) Run(ctx context.Context, req sandbox.RunRequest) (*sandbox.RunResult, error) {
	if req.Timeout == 0 {
		req.Timeout = sandbox.DefaultTimeout
	}
	if req.MemLimit == 0 {
		req.MemLimit = sandbox.DefaultMemLimit
	}
	if req.CPULimit == 0 {
		req.CPULimit = sandbox.DefaultCPULimit
	}

	img, err := r.resolveImage(ctx, req.Language, req.Image, req.Packages, false)
	if err != nil {
		return nil, err
	}

	if len(req.Packages) == 0 {
		if err := r.ensureImage(ctx, img); err != nil {
			return nil, err
		}
	}

	payload := map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("sandbox/docker: marshal payload: %w", err)
	}

	start := time.Now()

	var containerID string
	var fromPool bool

	if r.pool != nil && req.Image == "" && len(req.Packages) == 0 && !req.Network &&
		req.MemLimit == sandbox.DefaultMemLimit &&
		req.CPULimit == sandbox.DefaultCPULimit {
		if id := r.pool.Acquire(req.Language); id != "" {
			containerID = id
			fromPool = true
			slog.Info("sandbox/docker: pool acquired", "container", id[:12], "language", req.Language)
		}
	}

	if !fromPool {
		slog.Info("sandbox/docker: cold start (no pool)", "language", req.Language)
		cfg := &container.Config{
			Image:       img,
			Labels:      sandboxLabels(),
			AttachStdin: true,
			OpenStdin:   true,
			StdinOnce:   true,
			Tty:         false,
		}

		nanoCPUs := int64(req.CPULimit * 1e9)
		hostCfg := &container.HostConfig{
			Resources: container.Resources{
				Memory:    req.MemLimit,
				NanoCPUs:  nanoCPUs,
				PidsLimit: intPtr(256),
			},
			NetworkMode:    "none",
			AutoRemove:     false,
			ReadonlyRootfs: false,
		}

		if req.Network {
			hostCfg.NetworkMode = "bridge"
		}
		if r.runtime != "" {
			hostCfg.Runtime = r.runtime
		}

		resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
		if err != nil {
			return nil, fmt.Errorf("sandbox/docker: create container: %w", err)
		}
		containerID = resp.ID
	}

	defer func() {
		if fromPool {
			r.pool.Release(context.Background(), containerID)
		} else {
			rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = r.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
		}
	}()

	attachResp, err := r.cli.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox/docker: attach: %w", err)
	}
	defer attachResp.Close()

	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox/docker: start: %w", err)
	}

	_, _ = attachResp.Conn.Write(payloadBytes)
	_ = attachResp.CloseWrite()

	execCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	waitCh, errCh := r.cli.ContainerWait(execCtx, containerID, container.WaitConditionNotRunning)

	var exitCode int64
	select {
	case result := <-waitCh:
		exitCode = result.StatusCode
	case err := <-errCh:
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer killCancel()
		_ = r.cli.ContainerKill(killCtx, containerID, "SIGKILL")
		return nil, fmt.Errorf("sandbox/docker: wait: %w", err)
	case <-execCtx.Done():
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer killCancel()
		_ = r.cli.ContainerKill(killCtx, containerID, "SIGKILL")
		return nil, fmt.Errorf("sandbox/docker: execution timed out after %s", req.Timeout)
	}

	duration := time.Since(start)

	var stdoutBuf, stderrBuf bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, attachResp.Reader)

	stdout := strings.TrimSpace(stdoutBuf.String())
	stderr := strings.TrimSpace(stderrBuf.String())

	if stderr != "" {
		slog.Debug("sandbox/docker stderr", "container", containerID[:12], "stderr", stderr)
	}

	var output any
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &output); err != nil {
			output = stdout
		}
	}

	return &sandbox.RunResult{
		Output:   output,
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: int(exitCode),
		Duration: duration,
	}, nil
}

// StreamRun executes user code and streams stdout/stderr in real-time.
// The returned channel receives OutputEvents until closed (after exit event).
func (r *Runtime) StreamRun(ctx context.Context, req sandbox.RunRequest) (<-chan sandbox.OutputEvent, error) {
	if req.Timeout == 0 {
		req.Timeout = sandbox.DefaultTimeout
	}
	if req.MemLimit == 0 {
		req.MemLimit = sandbox.DefaultMemLimit
	}
	if req.CPULimit == 0 {
		req.CPULimit = sandbox.DefaultCPULimit
	}

	img, err := r.resolveImage(ctx, req.Language, req.Image, req.Packages, false)
	if err != nil {
		return nil, err
	}
	if len(req.Packages) == 0 {
		if err := r.ensureImage(ctx, img); err != nil {
			return nil, err
		}
	}

	payload := map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("sandbox/docker: marshal payload: %w", err)
	}

	start := time.Now()

	var containerID string
	var fromPool bool

	if r.pool != nil && req.Image == "" && len(req.Packages) == 0 && !req.Network &&
		req.MemLimit == sandbox.DefaultMemLimit &&
		req.CPULimit == sandbox.DefaultCPULimit {
		if id := r.pool.Acquire(req.Language); id != "" {
			containerID = id
			fromPool = true
			slog.Info("sandbox/docker: pool acquired", "container", id[:12], "language", req.Language, "stream", true)
		}
	}

	if !fromPool {
		cfg := &container.Config{
			Image:       img,
			Labels:      sandboxLabels(),
			AttachStdin: true,
			OpenStdin:   true,
			StdinOnce:   true,
			Tty:         false,
		}
		nanoCPUs := int64(req.CPULimit * 1e9)
		hostCfg := &container.HostConfig{
			Resources: container.Resources{
				Memory:    req.MemLimit,
				NanoCPUs:  nanoCPUs,
				PidsLimit: intPtr(256),
			},
			NetworkMode:    "none",
			AutoRemove:     false,
			ReadonlyRootfs: false,
		}
		if req.Network {
			hostCfg.NetworkMode = "bridge"
		}
		if r.runtime != "" {
			hostCfg.Runtime = r.runtime
		}
		resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
		if err != nil {
			return nil, fmt.Errorf("sandbox/docker: create container: %w", err)
		}
		containerID = resp.ID
	}

	attachResp, err := r.cli.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		r.cleanupContainer(fromPool, containerID)
		return nil, fmt.Errorf("sandbox/docker: attach: %w", err)
	}

	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		attachResp.Close()
		r.cleanupContainer(fromPool, containerID)
		return nil, fmt.Errorf("sandbox/docker: start: %w", err)
	}

	_, _ = attachResp.Conn.Write(payloadBytes)
	_ = attachResp.CloseWrite()

	ch := make(chan sandbox.OutputEvent, 64)

	go func() {
		defer close(ch)
		defer attachResp.Close()
		defer r.cleanupContainer(fromPool, containerID)

		stdoutW := &sandbox.StreamWriter{Stream: "stdout", Ch: ch}
		stderrW := &sandbox.StreamWriter{Stream: "stderr", Ch: ch}
		_, _ = stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader)

		execCtx, cancel := context.WithTimeout(ctx, req.Timeout)
		defer cancel()

		waitCh, errCh := r.cli.ContainerWait(execCtx, containerID, container.WaitConditionNotRunning)

		var exitCode int64
		select {
		case result := <-waitCh:
			exitCode = result.StatusCode
		case err := <-errCh:
			ch <- sandbox.OutputEvent{Stream: "exit", Error: fmt.Sprintf("wait: %v", err)}
			return
		case <-execCtx.Done():
			killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer killCancel()
			_ = r.cli.ContainerKill(killCtx, containerID, "SIGKILL")
			ch <- sandbox.OutputEvent{Stream: "exit", Error: fmt.Sprintf("execution timed out after %s", req.Timeout)}
			return
		}

		ch <- sandbox.OutputEvent{
			Stream:   "exit",
			ExitCode: int(exitCode),
			Duration: time.Since(start).Round(time.Millisecond).String(),
		}
	}()

	return ch, nil
}

// cleanupContainer removes a container, using pool.Release for pooled containers.
func (r *Runtime) cleanupContainer(fromPool bool, containerID string) {
	if fromPool {
		r.pool.Release(context.Background(), containerID)
	} else {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
	}
}

// ensureImage pulls the image if not present locally.
func (r *Runtime) ensureImage(ctx context.Context, img string) error {
	_, err := r.cli.ImageInspect(ctx, img)
	if err == nil {
		return nil
	}

	slog.Info("sandbox/docker: pulling image", "image", img)
	reader, err := r.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("sandbox/docker: pull %s: %w", img, err)
	}
	defer func() { _ = reader.Close() }()
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

// StartDebug creates a debug-enabled sandbox container with port binding for
// DAP/CDP communication.
func (r *Runtime) StartDebug(ctx context.Context, req sandbox.DebugRequest) (*sandbox.DebugSession, error) {
	if r.portAlloc == nil {
		return nil, fmt.Errorf("sandbox/docker: debug ports not configured")
	}

	img, err := r.resolveImage(ctx, req.Language, req.Image, req.Packages, true)
	if err != nil {
		return nil, err
	}

	containerDebugPort := sandbox.DebugPortForLanguage(req.Language)
	if containerDebugPort == 0 {
		return nil, fmt.Errorf("sandbox/docker: no debug port defined for %s", req.Language)
	}

	if len(req.Packages) == 0 {
		if err := r.ensureImage(ctx, img); err != nil {
			return nil, err
		}
	}

	hostPort, err := r.portAlloc.Acquire()
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		r.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox/docker: marshal payload: %w", err)
	}

	containerPort := nat.Port(fmt.Sprintf("%d/tcp", containerDebugPort))
	cfg := &container.Config{
		Image:        img,
		Labels:       sandboxLabels(),
		AttachStdin:  true,
		OpenStdin:    true,
		StdinOnce:    true,
		Tty:          false,
		Env:          []string{"SANDBOX_DEBUG=1"},
		ExposedPorts: nat.PortSet{containerPort: struct{}{}},
	}

	memLimit := req.MemLimit
	if memLimit == 0 {
		memLimit = 256 * 1024 * 1024
	}
	cpuLimit := req.CPULimit
	if cpuLimit == 0 {
		cpuLimit = 1.0
	}
	nanoCPUs := int64(cpuLimit * 1e9)

	hostCfg := &container.HostConfig{
		Resources: container.Resources{
			Memory:    memLimit,
			NanoCPUs:  nanoCPUs,
			PidsLimit: intPtr(256),
		},
		NetworkMode: "bridge",
		AutoRemove:  false,
		PortBindings: nat.PortMap{
			containerPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: fmt.Sprintf("%d", hostPort)}},
		},
	}

	if r.runtime != "" {
		hostCfg.Runtime = r.runtime
	}

	resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		r.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox/docker: create debug container: %w", err)
	}

	attachResp, err := r.cli.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		rmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.cli.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true})
		r.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox/docker: attach debug: %w", err)
	}

	if err := r.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		attachResp.Close()
		rmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.cli.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true})
		r.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox/docker: start debug: %w", err)
	}

	_, _ = attachResp.Conn.Write(payloadBytes)
	_ = attachResp.CloseWrite()

	go func() {
		defer attachResp.Close()
		_, _ = io.Copy(io.Discard, attachResp.Reader)
	}()

	sessionID := resp.ID[:12]
	session := &sandbox.DebugSession{
		ID:          sessionID,
		ContainerID: resp.ID,
		Language:    req.Language,
		DebugPort:   hostPort,
		Status:      "starting",
		CreatedAt:   time.Now(),
	}

	r.debugMu.Lock()
	r.debugSessions[sessionID] = session
	r.debugMu.Unlock()

	slog.Info("sandbox/docker: debug session started",
		"session", sessionID,
		"language", req.Language,
		"host_port", hostPort,
		"container_port", containerDebugPort,
	)

	return session, nil
}

// StopDebug kills a debug container and releases its port.
func (r *Runtime) StopDebug(sessionID string) error {
	r.debugMu.Lock()
	session, ok := r.debugSessions[sessionID]
	if ok {
		delete(r.debugSessions, sessionID)
	}
	r.debugMu.Unlock()

	if !ok {
		return fmt.Errorf("sandbox/docker: debug session %s not found", sessionID)
	}

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = r.cli.ContainerKill(killCtx, session.ContainerID, "SIGKILL")
	_ = r.cli.ContainerRemove(killCtx, session.ContainerID, container.RemoveOptions{Force: true})

	if r.portAlloc != nil {
		r.portAlloc.Release(session.DebugPort)
	}

	slog.Info("sandbox/docker: debug session stopped", "session", sessionID)
	return nil
}

// DebugPort returns the host debug port for a session.
func (r *Runtime) DebugPort(sessionID string) (int, error) {
	r.debugMu.Lock()
	defer r.debugMu.Unlock()

	session, ok := r.debugSessions[sessionID]
	if !ok {
		return 0, fmt.Errorf("sandbox/docker: debug session %s not found", sessionID)
	}
	return session.DebugPort, nil
}

// GetDebugSession returns the session info, if it exists.
func (r *Runtime) GetDebugSession(sessionID string) (*sandbox.DebugSession, bool) {
	r.debugMu.Lock()
	defer r.debugMu.Unlock()
	s, ok := r.debugSessions[sessionID]
	return s, ok
}

func intPtr(v int64) *int64 { return &v }
