package sandbox

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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// sandboxLabel is applied to all sandbox containers for orphan cleanup.
const sandboxLabel = "immaiwin.sandbox"

// Manager creates and runs sandbox containers via the Docker daemon.
type Manager struct {
	cli           client.APIClient
	runtime       string // OCI runtime override (e.g. "runsc" for gVisor); empty = default
	pool          *Pool
	builder       *ImageBuilder
	portAlloc     *PortAllocator
	debugMu       sync.Mutex
	debugSessions map[string]*DebugSession
}

// ManagerOption configures Manager.
type ManagerOption func(*Manager)

// WithRuntime sets the OCI runtime (e.g. "runsc" for gVisor).
func WithRuntime(rt string) ManagerOption {
	return func(m *Manager) { m.runtime = rt }
}

// WithPool attaches a warm container pool.
func WithPool(p *Pool) ManagerOption {
	return func(m *Manager) { m.pool = p }
}

// WithDebugPorts configures the debug port range for interactive debugging.
func WithDebugPorts(start, end int) ManagerOption {
	return func(m *Manager) {
		m.portAlloc = NewPortAllocator(start, end)
	}
}

// NewManager creates a sandbox Manager. It connects to the Docker daemon using
// environment defaults (DOCKER_HOST, etc.).
func NewManager(opts ...ManagerOption) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("sandbox: docker client: %w", err)
	}

	m := &Manager{
		cli:           cli,
		builder:       NewImageBuilder(cli),
		debugSessions: make(map[string]*DebugSession),
	}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// DockerClient exposes the underlying Docker client (e.g. for pool creation).
func (m *Manager) DockerClient() client.APIClient {
	return m.cli
}

// SetPool attaches a warm container pool post-construction.
func (m *Manager) SetPool(p *Pool) {
	m.pool = p
}

// Close releases the Docker client.
func (m *Manager) Close() error {
	return m.cli.Close()
}

// resolveImage determines the final image tag for a run.
// Priority: custom_image > packages (auto-build) > default base.
func (m *Manager) resolveImage(ctx context.Context, lang Language, customImage string, packages []string, debug bool) (string, error) {
	// 1. Custom image wins
	if customImage != "" {
		return customImage, nil
	}

	// 2. Determine base image
	var base string
	if debug {
		base = DebugImageForLanguage(lang)
	} else {
		base = ImageForLanguage(lang)
	}
	if base == "" {
		return "", fmt.Errorf("sandbox: unsupported language: %s", lang)
	}

	// 3. If packages specified, auto-build on top of base
	if len(packages) > 0 {
		return m.builder.BuildOrReuse(ctx, lang, base, packages, debug)
	}

	return base, nil
}

// CleanupOrphans removes any sandbox containers left over from a previous
// crashed session. Call once at startup before pool.Warm().
func (m *Manager) CleanupOrphans(ctx context.Context) {
	f := filters.NewArgs(filters.Arg("label", sandboxLabel+"=true"))
	containers, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: f,
	})
	if err != nil {
		slog.Warn("sandbox: orphan cleanup list failed", "err", err)
		return
	}
	if len(containers) == 0 {
		return
	}
	slog.Info("sandbox: cleaning up orphan containers", "count", len(containers))
	for _, c := range containers {
		rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = m.cli.ContainerRemove(rmCtx, c.ID, container.RemoveOptions{Force: true})
		cancel()
		slog.Info("sandbox: removed orphan", "container", c.ID[:12])
	}
}

// sandboxLabels returns the label map for sandbox containers.
func sandboxLabels() map[string]string {
	return map[string]string{sandboxLabel: "true"}
}

// Run executes user code in an isolated container and returns the result.
//
// Flow:
//  1. Create container with resource limits and network isolation.
//  2. Write input.json + script into the container.
//  3. Start container, wait for exit.
//  4. Read stdout (output JSON), stderr (logs).
//  5. Remove container.
func (m *Manager) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.MemLimit == 0 {
		req.MemLimit = DefaultMemLimit
	}
	if req.CPULimit == 0 {
		req.CPULimit = DefaultCPULimit
	}

	img, err := m.resolveImage(ctx, req.Language, req.Image, req.Packages, false)
	if err != nil {
		return nil, err
	}

	// Ensure image exists locally (for custom/base images; auto-built already local)
	if len(req.Packages) == 0 {
		if err := m.ensureImage(ctx, img); err != nil {
			return nil, err
		}
	}

	// Marshal the payload that the entrypoint reads from stdin.
	payload := map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("sandbox: marshal payload: %w", err)
	}

	start := time.Now()

	// Try warm pool first — only when limits match defaults, no network, no custom image, no packages
	var containerID string
	var fromPool bool

	if m.pool != nil && req.Image == "" && len(req.Packages) == 0 && !req.Network &&
		req.MemLimit == DefaultMemLimit &&
		req.CPULimit == DefaultCPULimit {
		if id := m.pool.Acquire(req.Language); id != "" {
			containerID = id
			fromPool = true
			slog.Info("sandbox: pool acquired", "container", id[:12], "language", req.Language)
		}
	}

	if !fromPool {
		slog.Info("sandbox: cold start (no pool)", "language", req.Language)
		// Container config
		cfg := &container.Config{
			Image:       img,
			Labels:      sandboxLabels(),
			AttachStdin: true,
			OpenStdin:   true,
			StdinOnce:   true,
			Tty:         false,
		}

		// Host config — resource limits + isolation
		nanoCPUs := int64(req.CPULimit * 1e9)
		hostCfg := &container.HostConfig{
			Resources: container.Resources{
				Memory:    req.MemLimit,
				NanoCPUs:  nanoCPUs,
				PidsLimit: intPtr(256), // Go/Rust compilers need many child processes
			},
			NetworkMode:    "none",
			AutoRemove:     false,
			ReadonlyRootfs: false, // entrypoint may need /tmp
		}

		if req.Network {
			hostCfg.NetworkMode = "bridge"
		}
		if m.runtime != "" {
			hostCfg.Runtime = m.runtime
		}

		resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
		if err != nil {
			return nil, fmt.Errorf("sandbox: create container: %w", err)
		}
		containerID = resp.ID
	}

	defer func() {
		if fromPool {
			m.pool.Release(context.Background(), containerID)
		} else {
			rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = m.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
		}
	}()

	// Attach stdin to feed payload
	attachResp, err := m.cli.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: attach: %w", err)
	}
	defer attachResp.Close()

	// Start container
	if err := m.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("sandbox: start: %w", err)
	}

	// Write payload to stdin then close
	_, _ = attachResp.Conn.Write(payloadBytes)
	_ = attachResp.CloseWrite()

	// Wait with timeout
	execCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	waitCh, errCh := m.cli.ContainerWait(execCtx, containerID, container.WaitConditionNotRunning)

	var exitCode int64
	select {
	case result := <-waitCh:
		exitCode = result.StatusCode
	case err := <-errCh:
		// Timeout or error — kill container
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer killCancel()
		_ = m.cli.ContainerKill(killCtx, containerID, "SIGKILL")
		return nil, fmt.Errorf("sandbox: wait: %w", err)
	case <-execCtx.Done():
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer killCancel()
		_ = m.cli.ContainerKill(killCtx, containerID, "SIGKILL")
		return nil, fmt.Errorf("sandbox: execution timed out after %s", req.Timeout)
	}

	duration := time.Since(start)

	// Demux stdout/stderr from multiplexed stream
	var stdoutBuf, stderrBuf bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdoutBuf, &stderrBuf, attachResp.Reader)

	stdout := strings.TrimSpace(stdoutBuf.String())
	stderr := strings.TrimSpace(stderrBuf.String())

	if stderr != "" {
		slog.Debug("sandbox stderr", "container", containerID[:12], "stderr", stderr)
	}

	// Parse output from stdout — entrypoint writes JSON to stdout
	var output any
	if stdout != "" {
		if err := json.Unmarshal([]byte(stdout), &output); err != nil {
			// If stdout isn't valid JSON, treat entire stdout as string output
			output = stdout
		}
	}

	return &RunResult{
		Output:   output,
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: int(exitCode),
		Duration: duration,
	}, nil
}

// streamWriter implements io.Writer and sends each write as an OutputEvent.
type streamWriter struct {
	stream string
	ch     chan<- OutputEvent
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.ch <- OutputEvent{Stream: w.stream, Data: string(p)}
	return len(p), nil
}

// StreamRun executes user code and streams stdout/stderr in real-time.
// The returned channel receives OutputEvents until closed (after exit event).
func (m *Manager) StreamRun(ctx context.Context, req RunRequest) (<-chan OutputEvent, error) {
	if req.Timeout == 0 {
		req.Timeout = DefaultTimeout
	}
	if req.MemLimit == 0 {
		req.MemLimit = DefaultMemLimit
	}
	if req.CPULimit == 0 {
		req.CPULimit = DefaultCPULimit
	}

	img, err := m.resolveImage(ctx, req.Language, req.Image, req.Packages, false)
	if err != nil {
		return nil, err
	}
	if len(req.Packages) == 0 {
		if err := m.ensureImage(ctx, img); err != nil {
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
		return nil, fmt.Errorf("sandbox: marshal payload: %w", err)
	}

	start := time.Now()

	// Try pool — skip for custom images and packages (pool keyed by Language)
	var containerID string
	var fromPool bool

	if m.pool != nil && req.Image == "" && len(req.Packages) == 0 && !req.Network &&
		req.MemLimit == DefaultMemLimit &&
		req.CPULimit == DefaultCPULimit {
		if id := m.pool.Acquire(req.Language); id != "" {
			containerID = id
			fromPool = true
			slog.Info("sandbox: pool acquired", "container", id[:12], "language", req.Language, "stream", true)
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
		if m.runtime != "" {
			hostCfg.Runtime = m.runtime
		}
		resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
		if err != nil {
			return nil, fmt.Errorf("sandbox: create container: %w", err)
		}
		containerID = resp.ID
	}

	attachResp, err := m.cli.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		m.cleanupContainer(fromPool, containerID)
		return nil, fmt.Errorf("sandbox: attach: %w", err)
	}

	if err := m.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		attachResp.Close()
		m.cleanupContainer(fromPool, containerID)
		return nil, fmt.Errorf("sandbox: start: %w", err)
	}

	_, _ = attachResp.Conn.Write(payloadBytes)
	_ = attachResp.CloseWrite()

	ch := make(chan OutputEvent, 64)

	go func() {
		defer close(ch)
		defer attachResp.Close()
		defer m.cleanupContainer(fromPool, containerID)

		// Stream stdout/stderr in real-time
		stdoutW := &streamWriter{stream: "stdout", ch: ch}
		stderrW := &streamWriter{stream: "stderr", ch: ch}
		_, _ = stdcopy.StdCopy(stdoutW, stderrW, attachResp.Reader)

		// Wait for exit
		execCtx, cancel := context.WithTimeout(ctx, req.Timeout)
		defer cancel()

		waitCh, errCh := m.cli.ContainerWait(execCtx, containerID, container.WaitConditionNotRunning)

		var exitCode int64
		select {
		case result := <-waitCh:
			exitCode = result.StatusCode
		case err := <-errCh:
			ch <- OutputEvent{Stream: "exit", Error: fmt.Sprintf("wait: %v", err)}
			return
		case <-execCtx.Done():
			killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer killCancel()
			_ = m.cli.ContainerKill(killCtx, containerID, "SIGKILL")
			ch <- OutputEvent{Stream: "exit", Error: fmt.Sprintf("execution timed out after %s", req.Timeout)}
			return
		}

		ch <- OutputEvent{
			Stream:   "exit",
			ExitCode: int(exitCode),
			Duration: time.Since(start).Round(time.Millisecond).String(),
		}
	}()

	return ch, nil
}

// cleanupContainer removes a container, using pool.Release for pooled containers.
func (m *Manager) cleanupContainer(fromPool bool, containerID string) {
	if fromPool {
		m.pool.Release(context.Background(), containerID)
	} else {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.cli.ContainerRemove(rmCtx, containerID, container.RemoveOptions{Force: true})
	}
}

// ensureImage pulls the image if not present locally.
func (m *Manager) ensureImage(ctx context.Context, img string) error {
	_, _, err := m.cli.ImageInspectWithRaw(ctx, img)
	if err == nil {
		return nil // already present
	}

	slog.Info("sandbox: pulling image", "image", img)
	reader, err := m.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("sandbox: pull %s: %w", img, err)
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	return nil
}

// StartDebug creates a debug-enabled sandbox container with port binding for
// DAP/CDP communication. The container runs with SANDBOX_DEBUG=1 env and the
// debug port mapped from container to host.
func (m *Manager) StartDebug(ctx context.Context, req DebugRequest) (*DebugSession, error) {
	if m.portAlloc == nil {
		return nil, fmt.Errorf("sandbox: debug ports not configured — use WithDebugPorts option")
	}

	// Resolve image: custom_image > packages (auto-build) > debug base
	img, err := m.resolveImage(ctx, req.Language, req.Image, req.Packages, true)
	if err != nil {
		return nil, err
	}

	containerDebugPort := DebugPortForLanguage(req.Language)
	if containerDebugPort == 0 {
		return nil, fmt.Errorf("sandbox: no debug port defined for %s", req.Language)
	}

	if len(req.Packages) == 0 {
		if err := m.ensureImage(ctx, img); err != nil {
			return nil, err
		}
	}

	hostPort, err := m.portAlloc.Acquire()
	if err != nil {
		return nil, err
	}

	// Marshal payload for stdin
	payload := map[string]any{
		"code":    req.Code,
		"input":   req.Input,
		"context": req.Context,
		"params":  req.Params,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		m.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox: marshal payload: %w", err)
	}

	// Container config with debug env
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

	// Resource limits — more generous for debug (longer timeout, more memory)
	memLimit := req.MemLimit
	if memLimit == 0 {
		memLimit = 256 * 1024 * 1024 // 256MB for debug
	}
	cpuLimit := req.CPULimit
	if cpuLimit == 0 {
		cpuLimit = 1.0 // 1 core for debug
	}
	nanoCPUs := int64(cpuLimit * 1e9)

	hostCfg := &container.HostConfig{
		Resources: container.Resources{
			Memory:    memLimit,
			NanoCPUs:  nanoCPUs,
			PidsLimit: intPtr(256),
		},
		NetworkMode: "bridge", // debug needs network for port binding
		AutoRemove:  false,
		PortBindings: nat.PortMap{
			containerPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: fmt.Sprintf("%d", hostPort)}},
		},
	}

	if m.runtime != "" {
		hostCfg.Runtime = m.runtime
	}

	resp, err := m.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		m.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox: create debug container: %w", err)
	}

	// Attach to feed payload via stdin
	attachResp, err := m.cli.ContainerAttach(ctx, resp.ID, container.AttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	if err != nil {
		rmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.cli.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true})
		m.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox: attach debug: %w", err)
	}

	// Start container
	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		attachResp.Close()
		rmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.cli.ContainerRemove(rmCtx, resp.ID, container.RemoveOptions{Force: true})
		m.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox: start debug: %w", err)
	}

	// Write payload then close stdin
	_, _ = attachResp.Conn.Write(payloadBytes)
	_ = attachResp.CloseWrite()

	// Drain attach output in background (debug output goes via DAP/CDP, not stdout)
	go func() {
		defer attachResp.Close()
		_, _ = io.Copy(io.Discard, attachResp.Reader)
	}()

	sessionID := resp.ID[:12]
	session := &DebugSession{
		ID:          sessionID,
		ContainerID: resp.ID,
		Language:    req.Language,
		DebugPort:   hostPort,
		Status:      "starting",
		CreatedAt:   time.Now(),
	}

	m.debugMu.Lock()
	m.debugSessions[sessionID] = session
	m.debugMu.Unlock()

	slog.Info("sandbox: debug session started",
		"session", sessionID,
		"language", req.Language,
		"host_port", hostPort,
		"container_port", containerDebugPort,
	)

	return session, nil
}

// StopDebug kills a debug container and releases its port.
func (m *Manager) StopDebug(sessionID string) error {
	m.debugMu.Lock()
	session, ok := m.debugSessions[sessionID]
	if ok {
		delete(m.debugSessions, sessionID)
	}
	m.debugMu.Unlock()

	if !ok {
		return fmt.Errorf("sandbox: debug session %s not found", sessionID)
	}

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = m.cli.ContainerKill(killCtx, session.ContainerID, "SIGKILL")
	_ = m.cli.ContainerRemove(killCtx, session.ContainerID, container.RemoveOptions{Force: true})

	if m.portAlloc != nil {
		m.portAlloc.Release(session.DebugPort)
	}

	slog.Info("sandbox: debug session stopped", "session", sessionID)
	return nil
}

// DebugPort returns the host debug port for a session.
func (m *Manager) DebugPort(sessionID string) (int, error) {
	m.debugMu.Lock()
	defer m.debugMu.Unlock()

	session, ok := m.debugSessions[sessionID]
	if !ok {
		return 0, fmt.Errorf("sandbox: debug session %s not found", sessionID)
	}
	return session.DebugPort, nil
}

// DebugSession returns the session info, if it exists.
func (m *Manager) GetDebugSession(sessionID string) (*DebugSession, bool) {
	m.debugMu.Lock()
	defer m.debugMu.Unlock()
	s, ok := m.debugSessions[sessionID]
	return s, ok
}

func intPtr(v int64) *int64 { return &v }
