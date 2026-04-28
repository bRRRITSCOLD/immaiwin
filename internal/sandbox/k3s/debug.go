package k3s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// StartDebug spawns a debug-enabled pod, attaches stdin to feed the JSON
// payload, and exposes the language debug port (debugpy 5678 / node 9229) via
// SPDY port-forward bound to a localhost port from PortAllocator.
//
// The returned DebugSession.DebugPort is the local TCP port. Existing DAP/CDP
// clients dial localhost:DebugPort exactly as with the Docker backend.
func (r *Runtime) StartDebug(ctx context.Context, req sandbox.DebugRequest) (*sandbox.DebugSession, error) {
	if r.portAlloc == nil {
		return nil, fmt.Errorf("sandbox/k3s: debug ports not configured")
	}

	containerDebugPort := sandbox.DebugPortForLanguage(req.Language)
	if containerDebugPort == 0 {
		return nil, fmt.Errorf("sandbox/k3s: no debug port defined for %s", req.Language)
	}

	// Debug image (registry-prefixed; builds package image if Packages set)
	image, err := r.resolveImage(ctx, req.RunRequest, true)
	if err != nil {
		return nil, err
	}

	hostPort, err := r.portAlloc.Acquire()
	if err != nil {
		return nil, err
	}

	// Adjust resource limits — debug needs more headroom than Run defaults.
	debugReq := req.RunRequest
	if debugReq.MemLimit == 0 {
		debugReq.MemLimit = 256 * 1024 * 1024
	}
	if debugReq.CPULimit == 0 {
		debugReq.CPULimit = 1.0
	}
	if debugReq.Timeout == 0 {
		debugReq.Timeout = 60 * time.Minute // long-running interactive session
	}
	debugReq.Network = true // egress allowed (entrypoint protocol unchanged)

	pod := r.buildPod(debugReq, image, true)
	created, err := r.clientset.CoreV1().Pods(r.ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		r.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox/k3s: create debug pod: %w", err)
	}

	if err := waitPodRunning(ctx, r.clientset, r.ns, created.Name, 90*time.Second); err != nil {
		deletePod(context.Background(), r.clientset, r.ns, created.Name)
		r.portAlloc.Release(hostPort)
		return nil, err
	}

	// Begin attach in the background — feeds JSON payload to entrypoint.
	// debugpy.listen() inside the pod opens once stdin payload is consumed.
	payload, err := json.Marshal(map[string]any{
		"code":    debugReq.Code,
		"input":   debugReq.Input,
		"context": debugReq.Context,
		"params":  debugReq.Params,
	})
	if err != nil {
		deletePod(context.Background(), r.clientset, r.ns, created.Name)
		r.portAlloc.Release(hostPort)
		return nil, fmt.Errorf("sandbox/k3s: marshal payload: %w", err)
	}

	if err := r.startDebugAttach(ctx, created.Name, payload); err != nil {
		deletePod(context.Background(), r.clientset, r.ns, created.Name)
		r.portAlloc.Release(hostPort)
		return nil, err
	}

	// Open SPDY port-forward to expose container debug port locally.
	pfCtx, pfCancel := context.WithTimeout(ctx, 30*time.Second)
	defer pfCancel()
	pf, err := r.startPortForward(pfCtx, created.Name, hostPort, containerDebugPort)
	if err != nil {
		deletePod(context.Background(), r.clientset, r.ns, created.Name)
		r.portAlloc.Release(hostPort)
		return nil, err
	}

	sessionID := created.Name[len("sandbox-debug-"):]
	if len(sessionID) > 12 {
		sessionID = sessionID[:12]
	}
	session := &sandbox.DebugSession{
		ID:          sessionID,
		ContainerID: created.Name, // pod name doubles as identifier
		Language:    req.Language,
		DebugPort:   hostPort,
		Status:      "starting",
		CreatedAt:   time.Now(),
	}

	r.debugMu.Lock()
	r.debugSessions[sessionID] = &debugSessionEntry{
		session: session,
		stopPF:  pf.stopCh,
	}
	r.debugMu.Unlock()

	slog.Info("sandbox/k3s: debug session started",
		"session", sessionID,
		"language", req.Language,
		"host_port", hostPort,
		"container_port", containerDebugPort,
		"pod", created.Name,
	)

	return session, nil
}

// debugLogWriter forwards container output to slog, line-by-line, tagged with
// the pod name + which std stream produced it. Used during debug sessions
// where output is normally noise but invaluable when something goes wrong.
type debugLogWriter struct {
	stream string
	pod    string
	buf    []byte
}

func (w *debugLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := -1
		for i, b := range w.buf {
			if b == '\n' {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		line := string(w.buf[:idx])
		w.buf = w.buf[idx+1:]
		if line != "" {
			slog.Info("sandbox/k3s: debug "+w.stream, "pod", w.pod, "line", line)
		}
	}
	return len(p), nil
}

// startDebugAttach opens an SPDY attach to the pod's main container in a
// background goroutine, writes the payload to stdin, and drains stdout/stderr.
// It does not block — attach lives for the full duration of the debug session.
func (r *Runtime) startDebugAttach(ctx context.Context, podName string, payload []byte) error {
	req := r.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(r.ns).
		Name(podName).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: "user",
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("sandbox/k3s: debug spdy executor: %w", err)
	}

	stdinR, stdinW := io.Pipe()
	go func() {
		defer stdinW.Close()
		_, _ = stdinW.Write(payload)
	}()

	// StreamWithContext blocks; run async. Capture stdout/stderr to slog so we
	// can see if the wrapper picks up stdin and spawns the inner debug process.
	go func() {
		stdoutW := &debugLogWriter{stream: "stdout", pod: podName}
		stderrW := &debugLogWriter{stream: "stderr", pod: podName}
		err := exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
			Stdin:  stdinR,
			Stdout: stdoutW,
			Stderr: stderrW,
			Tty:    false,
		})
		if err != nil && err != io.EOF {
			slog.Info("sandbox/k3s: debug attach ended", "err", err, "pod", podName)
		}
	}()

	return nil
}

// StopDebug terminates a debug session: closes the port-forward and deletes the pod.
func (r *Runtime) StopDebug(sessionID string) error {
	r.debugMu.Lock()
	entry, ok := r.debugSessions[sessionID]
	if ok {
		delete(r.debugSessions, sessionID)
	}
	r.debugMu.Unlock()

	if !ok {
		return fmt.Errorf("sandbox/k3s: debug session %s not found", sessionID)
	}

	// Close port-forward first to stop client traffic.
	close(entry.stopPF)

	// Delete pod (force, zero grace).
	delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deletePod(delCtx, r.clientset, r.ns, entry.session.ContainerID)

	if r.portAlloc != nil {
		r.portAlloc.Release(entry.session.DebugPort)
	}

	slog.Info("sandbox/k3s: debug session stopped", "session", sessionID)
	return nil
}

// DebugPort returns the local TCP port bound by the port-forward for a session.
func (r *Runtime) DebugPort(sessionID string) (int, error) {
	r.debugMu.Lock()
	defer r.debugMu.Unlock()
	entry, ok := r.debugSessions[sessionID]
	if !ok {
		return 0, fmt.Errorf("sandbox/k3s: debug session %s not found", sessionID)
	}
	return entry.session.DebugPort, nil
}

// GetDebugSession returns the session metadata, if any.
func (r *Runtime) GetDebugSession(sessionID string) (*sandbox.DebugSession, bool) {
	r.debugMu.Lock()
	defer r.debugMu.Unlock()
	entry, ok := r.debugSessions[sessionID]
	if !ok {
		return nil, false
	}
	return entry.session, true
}
