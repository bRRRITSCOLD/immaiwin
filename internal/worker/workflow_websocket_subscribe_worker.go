package worker

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/config"
	"github.com/bRRRITSCOLD/burrow/internal/mongodb"
	"github.com/bRRRITSCOLD/burrow/internal/rediss"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gorilla/websocket"
	"github.com/oklog/ulid/v2"
)

const wsSubSyncInterval = 30 * time.Second

// WorkflowWebSocketSubscribeWorker dials a user-supplied WebSocket
// endpoint per workflow whose trigger_type=="websocket", and
// dispatches a workflow run for every frame received. Provider-
// agnostic: auth lives on the Connection (bearer/header/query),
// frame routing is a JSON dot-path on data.event_path.
var WorkflowWebSocketSubscribeWorker = &workflowWebSocketSubscribeWorker{}

type workflowWebSocketSubscribeWorker struct{}

func (w *workflowWebSocketSubscribeWorker) Name() string { return "workflow-websocket-subscribe" }

type wsTriggerInfo struct {
	connectionID      string
	subscribePayload  string // optional JSON sent on connect (provider's subscribe handshake)
	eventPath         string // optional dot-path into the JSON frame; empty = whole frame
	heartbeatSeconds  int    // optional ping interval (default 30s; 0 disables)
}

type trackedWSSubscriber struct {
	cancel    context.CancelFunc
	updatedAt time.Time
	info      wsTriggerInfo
}

func wsInfoFromWorkflow(wf workflow.Workflow) (wsTriggerInfo, bool) {
	for _, n := range wf.Nodes {
		if n.Type != workflow.NodeTypeTrigger {
			continue
		}
		tt, _ := n.Data["trigger_type"].(string)
		if tt != "websocket" {
			continue
		}
		connID, _ := n.Data["connection_id"].(string)
		if strings.TrimSpace(connID) == "" {
			continue
		}
		info := wsTriggerInfo{
			connectionID:     connID,
			subscribePayload: getString(n.Data, "subscribe_payload"),
			eventPath:        strings.TrimSpace(getString(n.Data, "event_path")),
			heartbeatSeconds: 30,
		}
		if hs, ok := n.Data["heartbeat_seconds"].(float64); ok && hs >= 0 {
			info.heartbeatSeconds = int(hs)
		}
		return info, true
	}
	return wsTriggerInfo{}, false
}

func (w *workflowWebSocketSubscribeWorker) Run(ctx context.Context) error {
	cfg, err := config.Load(config.WithDotEnv(".env"))
	if err != nil {
		return fmt.Errorf("workflow-websocket-subscribe: load config: %w", err)
	}

	rc := rediss.New(cfg.Redis)
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			slog.Error("workflow-websocket-subscribe: close redis", "err", cerr)
		}
	}()

	mc, err := mongodb.New(ctx, cfg.MongoDB)
	if err != nil {
		return fmt.Errorf("workflow-websocket-subscribe: connect mongodb: %w", err)
	}
	defer func() {
		if derr := mc.Disconnect(ctx); derr != nil {
			slog.Error("workflow-websocket-subscribe: disconnect mongodb", "err", derr)
		}
	}()

	runRepo, err := mongodb.NewWorkflowRunRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-websocket-subscribe: create run repo: %w", err)
	}
	repo, err := mongodb.NewWorkflowRepository(ctx, mc.DB())
	if err != nil {
		return fmt.Errorf("workflow-websocket-subscribe: create workflow repo: %w", err)
	}

	var encKey []byte
	if trimmed := strings.TrimSpace(cfg.EncryptionKey); trimmed != "" {
		encKey, err = hex.DecodeString(trimmed)
		if err != nil || len(encKey) != 32 {
			return fmt.Errorf("workflow-websocket-subscribe: ENCRYPTION_KEY must be 64 hex chars (32 bytes): %v", err)
		}
	}
	connRepo, err := mongodb.NewConnectionRepository(ctx, mc.DB(), encKey)
	if err != nil {
		return fmt.Errorf("workflow-websocket-subscribe: create connection repo: %w", err)
	}

	tracked := make(map[string]*trackedWSSubscriber)
	syncWSSubscribers(ctx, repo, runRepo, rc, connRepo, tracked)

	ticker := time.NewTicker(wsSubSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			for wfID, ts := range tracked {
				ts.cancel()
				delete(tracked, wfID)
			}
			return nil
		case <-ticker.C:
			syncWSSubscribers(ctx, repo, runRepo, rc, connRepo, tracked)
		}
	}
}

func syncWSSubscribers(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	tracked map[string]*trackedWSSubscriber,
) {
	wfs, err := repo.List(ctx)
	if err != nil {
		slog.Error("workflow-websocket-subscribe: list workflows", "err", err)
		return
	}

	type activeEntry struct {
		info      wsTriggerInfo
		updatedAt time.Time
		name      string
	}
	active := make(map[string]activeEntry)
	for _, wf := range wfs {
		// Skip disabled workflows — the diff-and-cancel logic below
		// tears down the connection on the next pass. Frames pushed
		// while the workflow is disabled are LOST (WebSocket has no
		// replay; same caveat as Redis pub/sub).
		if !wf.Enabled {
			continue
		}
		info, ok := wsInfoFromWorkflow(wf)
		if !ok {
			continue
		}
		active[wf.ID] = activeEntry{info: info, updatedAt: wf.UpdatedAt, name: wf.Name}
	}

	for wfID, entry := range active {
		existing, ok := tracked[wfID]
		if ok && existing.updatedAt.Equal(entry.updatedAt) {
			continue
		}
		if ok {
			existing.cancel()
			slog.Info("workflow-websocket-subscribe: stopped stale subscriber", "workflow", wfID, "name", entry.name)
		}
		subCtx, cancel := context.WithCancel(ctx)
		tracked[wfID] = &trackedWSSubscriber{cancel: cancel, updatedAt: entry.updatedAt, info: entry.info}
		go wsSubscribeLoop(subCtx, repo, runRepo, rc, connRepo, wfID, entry.name, entry.info)
		slog.Info("workflow-websocket-subscribe: started subscriber", "workflow", wfID, "name", entry.name, "connection_id", entry.info.connectionID)
	}

	for wfID, ts := range tracked {
		if _, ok := active[wfID]; !ok {
			ts.cancel()
			delete(tracked, wfID)
			slog.Info("workflow-websocket-subscribe: removed deleted/disabled subscriber", "workflow", wfID)
		}
	}
}

func wsSubscribeLoop(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	wfID, wfName string,
	info wsTriggerInfo,
) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := wsSubscribeOnce(ctx, repo, runRepo, rc, connRepo, wfID, wfName, info)
		if ctx.Err() != nil {
			return
		}
		attempt++
		delay := time.Duration(math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(60*time.Second)))
		slog.Warn("workflow-websocket-subscribe: connection dropped, reconnecting",
			"workflow", wfID, "name", wfName, "err", err, "attempt", attempt, "backoff", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func wsSubscribeOnce(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	connRepo *mongodb.ConnectionRepository,
	wfID, wfName string,
	info wsTriggerInfo,
) error {
	conn, err := connRepo.GetByID(ctx, info.connectionID)
	if err != nil {
		return fmt.Errorf("resolve connection: %w", err)
	}
	if conn.Type != workflow.ConnectionTypeWebSocket {
		return fmt.Errorf("connection %q is %s, expected websocket", info.connectionID, conn.Type)
	}

	dialer, target, hdr, err := workflow.BuildWSDialer(conn.Config)
	if err != nil {
		return err
	}

	c, resp, err := dialer.DialContext(ctx, target, hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return fmt.Errorf("dial %s (status %d): %w", target, status, err)
	}
	defer c.Close() //nolint:errcheck

	if payload := strings.TrimSpace(info.subscribePayload); payload != "" {
		if werr := c.WriteMessage(websocket.TextMessage, []byte(payload)); werr != nil {
			return fmt.Errorf("send subscribe payload: %w", werr)
		}
	}

	// Heartbeat: send pings at the configured interval; the server
	// is expected to reply with pong (gorilla handles pong frames
	// automatically). Updates the read deadline on each pong so a
	// silent server is detected within ~2 intervals.
	if info.heartbeatSeconds > 0 {
		interval := time.Duration(info.heartbeatSeconds) * time.Second
		_ = c.SetReadDeadline(time.Now().Add(interval * 2))
		c.SetPongHandler(func(string) error {
			return c.SetReadDeadline(time.Now().Add(interval * 2))
		})
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if perr := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); perr != nil {
						return
					}
				}
			}
		}()
	}

	slog.Info("workflow-websocket-subscribe: connected", "workflow", wfID, "name", wfName, "target", target)

	for {
		if ctx.Err() != nil {
			return nil
		}
		_, data, rerr := c.ReadMessage()
		if rerr != nil {
			if errors.Is(rerr, context.Canceled) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read frame: %w", rerr)
		}
		processWSMessage(ctx, repo, runRepo, rc, wfID, wfName, info, data)
	}
}

// processWSMessage builds the trigger input + dispatches a run.
// Body shape exposed to the workflow:
//
//	{{input.raw}}        — raw frame bytes as string
//	{{input.json}}       — JSON-decoded frame (nil when frame isn't valid JSON)
//	{{input.extracted}}  — dot-path lookup into `json` per data.event_path
//	                       (nil when path absent / frame not JSON)
//
// event_path is a tiny dot-path ("data.payload"), no array index /
// JSONPath syntax — kept narrow on purpose; complex extraction
// belongs in a downstream node.
func processWSMessage(
	ctx context.Context,
	repo *mongodb.WorkflowRepository,
	runRepo *mongodb.WorkflowRunRepository,
	rc *rediss.Client,
	wfID, wfName string,
	info wsTriggerInfo,
	frame []byte,
) {
	raw := string(frame)
	var decoded any
	if jerr := json.Unmarshal(frame, &decoded); jerr != nil {
		decoded = nil
	}
	var extracted any
	if decoded != nil && info.eventPath != "" {
		extracted = lookupDotPath(decoded, info.eventPath)
	}

	body := map[string]any{
		"raw":       raw,
		"json":      decoded,
		"extracted": extracted,
	}

	wf, err := repo.GetByID(ctx, wfID)
	if err != nil {
		slog.Error("workflow-websocket-subscribe: fetch workflow", "workflow", wfID, "err", err)
		return
	}
	now := time.Now().UTC()
	rec := workflow.WorkflowRun{
		ID:           ulid.Make().String(),
		WorkflowID:   wf.ID,
		TenantID:     wf.TenantID,
		QueuedAt:     now,
		Status:       workflow.RunStatusQueued,
		Config: wf.Config,
		TriggerInput: body,
	}
	if _, cerr := runRepo.Create(ctx, rec); cerr != nil {
		slog.Error("workflow-websocket-subscribe: persist run rec failed", "workflow", wfID, "err", cerr)
		return
	}
	workflow.PublishWakeup(ctx, rc)
	slog.Info("workflow-websocket-subscribe: dispatched", "workflow", wfID, "name", wfName, "run_id", rec.ID)
}

// lookupDotPath walks a dotted key path through a JSON-decoded value.
// Tiny + intentionally narrow — no array index, no glob, no
// alternation. Returns nil for any missing segment so the workflow
// can branch with `{{input.extracted == null}}`.
func lookupDotPath(root any, path string) any {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		next, found := m[seg]
		if !found {
			return nil
		}
		cur = next
	}
	return cur
}
