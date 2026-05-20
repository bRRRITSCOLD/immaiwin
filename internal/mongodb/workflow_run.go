package mongodb

import (
	"context"
	"errors"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WorkflowRunRepository persists workflow run records.
type WorkflowRunRepository struct {
	col *mongo.Collection
}

// NewWorkflowRunRepository constructs the repo + ensures indexes.
func NewWorkflowRunRepository(ctx context.Context, db *mongo.Database) (*WorkflowRunRepository, error) {
	col := db.Collection("workflow_runs")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// queued_at = dispatch time (always set); listings sort by it
		// so queued runs appear immediately after dispatch.
		{Keys: bson.D{{Key: "workflow_id", Value: 1}, {Key: "queued_at", Value: -1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "queued_at", Value: -1}}},
		{Keys: bson.D{{Key: "status", Value: 1}}},
		// Powers ClaimLease's findAndModify predicate. Sparse on
		// lease_expires_at so docs that never had a lease (legacy /
		// unleased runs) don't bloat the index.
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lease_expires_at", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	return &WorkflowRunRepository{col: col}, nil
}

// Create inserts a new run record.
func (r *WorkflowRunRepository) Create(ctx context.Context, run workflow.WorkflowRun) (workflow.WorkflowRun, error) {
	if run.ID == "" {
		return workflow.WorkflowRun{}, errors.New("workflow_runs: ID required")
	}
	if _, err := r.col.InsertOne(ctx, run); err != nil {
		return workflow.WorkflowRun{}, err
	}
	return run, nil
}

// Get returns a single run by ID.
func (r *WorkflowRunRepository) Get(ctx context.Context, id string) (workflow.WorkflowRun, error) {
	var run workflow.WorkflowRun
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&run)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return workflow.WorkflowRun{}, errors.New("workflow_run not found")
		}
		return workflow.WorkflowRun{}, err
	}
	return run, nil
}

// Update writes the full run document. Used for batch updates after run
// completion. For live progress use AppendTrace + targeted updates.
func (r *WorkflowRunRepository) Update(ctx context.Context, run workflow.WorkflowRun) error {
	_, err := r.col.ReplaceOne(ctx, bson.M{"_id": run.ID}, run)
	return err
}

// CountInFlightForWorkflow returns the number of non-terminal runs
// for the given workflow_id. Cron trigger worker's skip_if_running
// option queries this before dispatching: if any prior tick is still
// running / queued / paused / awaiting approval, the cron worker
// skips this tick so concurrent runs of the same workflow don't
// stack.
func (r *WorkflowRunRepository) CountInFlightForWorkflow(ctx context.Context, workflowID string) (int64, error) {
	if workflowID == "" {
		return 0, errors.New("workflow_run count: workflow_id required")
	}
	return r.col.CountDocuments(ctx, bson.M{
		"workflow_id": workflowID,
		"status": bson.M{"$in": []string{
			string(workflow.RunStatusQueued),
			string(workflow.RunStatusRunning),
			string(workflow.RunStatusPaused),
			string(workflow.RunStatusPendingApproval),
		}},
	})
}

// SweepAbandonedNonTerminal flips every non-terminal run older than
// `olderThan` into a terminal `error` state with the supplied reason —
// EXCEPT runs that the lease pattern can recover. Used by the API boot
// path to clean up the legacy in-process tier (cron / rabbitmq /
// redis-sub triggers pre-3.4, sync /run requests pre-3.3) where the
// goroutines holding execution state are gone after a restart.
//
// Lease-aware gates (PR 3.5):
//
//  1. Skip runs with a live lease (`lease_owner != ""` AND
//     `lease_expires_at > now`). Another worker is heartbeating the
//     run; flipping it would race with `CheckpointExecutionState` /
//     `ExtendLease` and corrupt durable state.
//  2. Skip runs whose lease has expired but `execution_state != nil`.
//     The next `ClaimLease` tick will rehydrate from the checkpoint
//     and resume; flipping would discard committed BFS progress.
//
// What still gets flipped after the gates: legacy in-process runs
// (no lease ever taken, no execution_state ever written) that died
// when their owning pid restarted. Those are unrecoverable — the
// frontier + visited set + named-output map only ever lived in RAM.
//
// `olderThan` is a safety window so a parallel API process that's
// legitimately running fresh work doesn't get its records stomped.
// Anything started AFTER (boot_time - olderThan) is assumed to belong
// to a still-alive process and skipped. Single-pod deployments can
// pass a near-zero window; multi-pod deployments need wider margins.
// With the lease gates in (1) + (2) above, the window is now defense
// in depth rather than primary safety; still wired in case some
// future trigger source bypasses both lease + execution_state.
func (r *WorkflowRunRepository) SweepAbandonedNonTerminal(ctx context.Context, reason string, olderThan time.Duration) (int64, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-olderThan)
	filter := bson.M{
		"status": bson.M{"$in": []string{
			string(workflow.RunStatusQueued),
			string(workflow.RunStatusRunning),
			string(workflow.RunStatusPaused),
			string(workflow.RunStatusPendingApproval),
		}},
		"queued_at": bson.M{"$lt": cutoff},
		// Gate 1: skip live-lease runs. A run is live-leased when
		// lease_owner is set AND lease_expires_at is in the future.
		// The negation (sweep target) is the union: no owner OR
		// expired deadline. We express it as $or so a single missing
		// field short-circuits the candidate into the sweep set.
		"$or": []bson.M{
			{"lease_owner": bson.M{"$in": []any{nil, ""}}},
			{"lease_owner": bson.M{"$exists": false}},
			{"lease_expires_at": bson.M{"$lte": now}},
			{"lease_expires_at": bson.M{"$exists": false}},
		},
		// Gate 2: skip durable runs (execution_state set). The next
		// claim tick will rehydrate. `execution_state` is unset on
		// boot for legacy in-process runs and on terminal cleanup
		// for everything else, so this filter equals "the legacy
		// orphan tier and nothing else." `{field: nil}` matches both
		// missing-field and null-field per Mongo's documented quirk —
		// covers legacy rows that never had the field written.
		"execution_state": nil,
	}
	update := bson.M{
		"$set": bson.M{
			"status":      string(workflow.RunStatusError),
			"error":       reason,
			"finished_at": now,
		},
		"$unset": bson.M{
			"pending_approval": "",
			"paused_agent":     "",
		},
	}
	res, err := r.col.UpdateMany(ctx, filter, update)
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}

// SweepWorkerOrphansCollect catches runs the executor worker was
// driving but lost — a process crash mid-run, a debug-paused run
// whose worker died at the pre-exec breakpoint wait. Returns the
// run IDs of every flipped run so the caller can publish synthetic
// run_done events on burrow:run_events:<runID> for live canvas WS
// subscribers to flip terminal immediately.
//
// Filter is narrower than the boot-time SweepAbandonedNonTerminal
// because this runs PERIODICALLY in the executor worker:
//
//   - status IN {running, paused, pending_approval}.
//     `queued` is excluded — a queued run is awaiting first claim;
//     with concurrency limits a queued run can sit past the sweep
//     window while the worker is mid-flight on another run. That's
//     normal, not an orphan.
//   - lease_owner != "" AND lease_expires_at <= now. The run HAD a
//     worker (so we're not stomping unclaimed work), and that
//     worker is provably gone (lease lapsed).
//   - execution_state IS NULL. A durable run with a checkpoint
//     resumes cleanly on the next claim; flipping it would discard
//     committed BFS progress.
//   - queued_at < now - olderThan. Safety window.
//
// Two-step (Find → per-row Update with the same predicate) so the
// returned IDs reflect actual writes + a race with another
// worker's ClaimLease cannot corrupt state.
func (r *WorkflowRunRepository) SweepWorkerOrphansCollect(ctx context.Context, reason string, olderThan time.Duration) ([]string, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-olderThan)
	filter := bson.M{
		"status": bson.M{"$in": []string{
			string(workflow.RunStatusRunning),
			string(workflow.RunStatusPaused),
			string(workflow.RunStatusPendingApproval),
		}},
		"queued_at":        bson.M{"$lt": cutoff},
		"lease_owner":      bson.M{"$nin": []any{nil, ""}}, // run was actually claimed
		"lease_expires_at": bson.M{"$lte": now},            // and that lease has lapsed
		"execution_state":  nil,
	}
	cur, err := r.col.Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return nil, err
	}
	var docs []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		update := bson.M{
			"$set": bson.M{
				"status":      string(workflow.RunStatusError),
				"error":       reason,
				"finished_at": now,
			},
			"$unset": bson.M{
				"pending_approval": "",
				"paused_agent":     "",
			},
		}
		// Defensive: re-apply the same predicate so a run that
		// the caller's own claim loop reclaimed between Find and
		// Update doesn't get stomped.
		oneFilter := bson.M{"_id": d.ID}
		for k, v := range filter {
			if k == "_id" {
				continue
			}
			oneFilter[k] = v
		}
		res, err := r.col.UpdateOne(ctx, oneFilter, update)
		if err != nil {
			continue
		}
		if res.ModifiedCount > 0 {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

// List returns the most recent runs for a workflow, newest first.
func (r *WorkflowRunRepository) List(ctx context.Context, workflowID string, limit int) ([]workflow.WorkflowRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "queued_at", Value: -1}}).
		SetLimit(int64(limit))

	cur, err := r.col.Find(ctx, bson.M{"workflow_id": workflowID}, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()

	var runs []workflow.WorkflowRun
	if err := cur.All(ctx, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

// ListWithFilter implements workflow.WorkflowRunStore for the runs page.
// Empty filter fields are skipped; limit defaults to 50 (capped at 200).
func (r *WorkflowRunRepository) ListWithFilter(ctx context.Context, f workflow.RunFilter) ([]workflow.WorkflowRun, error) {
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	skip := f.Skip
	if skip < 0 {
		skip = 0
	}

	q := bson.M{}
	if f.TenantID != "" {
		q["tenant_id"] = f.TenantID
	}
	if f.WorkflowID != "" {
		q["workflow_id"] = f.WorkflowID
	}
	if f.Status != "" {
		q["status"] = f.Status
	}
	if !f.StartedAfter.IsZero() || !f.StartedBefore.IsZero() {
		startedQ := bson.M{}
		if !f.StartedAfter.IsZero() {
			startedQ["$gte"] = f.StartedAfter
		}
		if !f.StartedBefore.IsZero() {
			startedQ["$lte"] = f.StartedBefore
		}
		q["queued_at"] = startedQ
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "queued_at", Value: -1}}).
		SetLimit(int64(limit)).
		SetSkip(int64(skip))

	cur, err := r.col.Find(ctx, q, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()

	var runs []workflow.WorkflowRun
	if err := cur.All(ctx, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

// SumCostSince aggregates the cost of all runs for a workflow since `since`.
// Returns 0 when no matching runs (Mongo $sum on empty group). Used by the
// daily cap enforcement.
func (r *WorkflowRunRepository) SumCostSince(ctx context.Context, workflowID string, since time.Time) (float64, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{
			"workflow_id": workflowID,
			"queued_at":   bson.M{"$gte": since},
		}}},
		{{Key: "$group", Value: bson.M{
			"_id":   nil,
			"total": bson.M{"$sum": "$usage.cost_usd"},
		}}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var rows []struct {
		Total float64 `bson:"total"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].Total, nil
}

// RunMetricsFilter scopes the metrics aggregation. TenantID required
// (caller is responsible for setting it from the active ctx); Since/
// Until bound the started_at window. Empty bounds = unbounded.
type RunMetricsFilter struct {
	TenantID string
	Since    time.Time
	Until    time.Time
}

// WorkflowRollup is one workflow's contribution to the metrics view —
// run count + cost. Used for "top workflows by activity / cost" lists.
type WorkflowRollup struct {
	WorkflowID string  `bson:"_id"      json:"workflow_id"`
	Count      int64   `bson:"count"    json:"count"`
	CostUSD    float64 `bson:"cost_usd" json:"cost_usd"`
}

// StatusCount is one (status, count) pair from the by_status facet.
type StatusCount struct {
	Status string `bson:"_id"   json:"status"`
	Count  int64  `bson:"count" json:"count"`
}

// RunMetrics is the aggregate view returned by AggregateMetrics.
type RunMetrics struct {
	TotalRuns    int64            `json:"total_runs"`
	TotalCostUSD float64          `json:"total_cost_usd"`
	ByStatus     []StatusCount    `json:"by_status"`
	TopWorkflows []WorkflowRollup `json:"top_workflows"` // top 10 by run count
}

// AggregateMetrics returns a single-pass rollup of run counts + cost
// across status + workflow dimensions, scoped to the filter's tenant
// and started_at window. Uses $facet so all dimensions ride one
// pipeline scan over the bounded match — cheap enough for ad-hoc
// dashboard refreshes without a materialised view.
func (r *WorkflowRunRepository) AggregateMetrics(ctx context.Context, f RunMetricsFilter) (RunMetrics, error) {
	if f.TenantID == "" {
		return RunMetrics{}, errors.New("run_metrics: tenant_id required")
	}
	match := bson.M{"tenant_id": f.TenantID}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		startedQ := bson.M{}
		if !f.Since.IsZero() {
			startedQ["$gte"] = f.Since
		}
		if !f.Until.IsZero() {
			startedQ["$lte"] = f.Until
		}
		match["queued_at"] = startedQ
	}

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$facet", Value: bson.M{
			"totals": bson.A{
				bson.M{"$group": bson.M{
					"_id":      nil,
					"count":    bson.M{"$sum": 1},
					"cost_usd": bson.M{"$sum": "$usage.cost_usd"},
				}},
			},
			"by_status": bson.A{
				bson.M{"$group": bson.M{
					"_id":   "$status",
					"count": bson.M{"$sum": 1},
				}},
				bson.M{"$sort": bson.M{"count": -1}},
			},
			"top_workflows": bson.A{
				bson.M{"$group": bson.M{
					"_id":      "$workflow_id",
					"count":    bson.M{"$sum": 1},
					"cost_usd": bson.M{"$sum": "$usage.cost_usd"},
				}},
				bson.M{"$sort": bson.M{"count": -1}},
				bson.M{"$limit": 10},
			},
		}}},
	}
	cur, err := r.col.Aggregate(ctx, pipeline)
	if err != nil {
		return RunMetrics{}, err
	}
	defer func() { _ = cur.Close(ctx) }()

	var rows []struct {
		Totals []struct {
			Count   int64   `bson:"count"`
			CostUSD float64 `bson:"cost_usd"`
		} `bson:"totals"`
		ByStatus     []StatusCount    `bson:"by_status"`
		TopWorkflows []WorkflowRollup `bson:"top_workflows"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return RunMetrics{}, err
	}
	if len(rows) == 0 {
		return RunMetrics{}, nil
	}
	out := RunMetrics{
		ByStatus:     rows[0].ByStatus,
		TopWorkflows: rows[0].TopWorkflows,
	}
	if len(rows[0].Totals) > 0 {
		out.TotalRuns = rows[0].Totals[0].Count
		out.TotalCostUSD = rows[0].Totals[0].CostUSD
	}
	return out, nil
}

// AppendTrace atomically pushes a trace event to a run's agent_traces map.
// Mongo dot-notation on a map field: agent_traces.<nodeID>
func (r *WorkflowRunRepository) AppendTrace(ctx context.Context, runID, nodeID string, event workflow.TraceEvent) error {
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": runID},
		bson.M{"$push": bson.M{"agent_traces." + nodeID: event}},
	)
	return err
}

// ClaimLease atomically acquires a lease on the next claimable run.
// Predicate:
//   - status ∈ statuses
//   - lease_owner is missing OR empty OR lease_expires_at is missing OR <= now
//
// On success the same findAndModify writes lease_owner=workerID and
// lease_expires_at=now+leaseDur. ReturnDocument:After returns the
// post-update document so caller sees the new lease state.
//
// Returns (run, true, nil) on success; (zero, false, nil) when no
// candidate exists; (zero, false, err) on I/O failure. Never returns
// an unleased run on success.
func (r *WorkflowRunRepository) ClaimLease(ctx context.Context, workerID string, leaseDur time.Duration, statuses []workflow.RunStatus) (workflow.WorkflowRun, bool, error) {
	if workerID == "" {
		return workflow.WorkflowRun{}, false, errors.New("workflow run lease: workerID required")
	}
	if leaseDur <= 0 {
		return workflow.WorkflowRun{}, false, errors.New("workflow run lease: leaseDur must be > 0")
	}
	if len(statuses) == 0 {
		return workflow.WorkflowRun{}, false, errors.New("workflow run lease: statuses required")
	}
	now := time.Now().UTC()
	statusStrs := make([]string, len(statuses))
	for i, s := range statuses {
		statusStrs[i] = string(s)
	}
	// Claimable iff:
	//   1. Never been leased (fresh queued runs + legacy in-process
	//      tier). lease_owner empty/missing.
	//   2. Lease lapsed AND there's a checkpoint to resume from.
	//      Without `execution_state` we'd be restarting BFS from
	//      trigger — silent re-execution + (worse) blocks new
	//      queued runs from being claimed under concurrency=1 if
	//      the orphan keeps pausing. The orphan-sweep loop
	//      (SweepWorkerOrphansCollect) terminates such runs
	//      explicitly so the operator sees a clear error state.
	filter := bson.M{
		"status": bson.M{"$in": statusStrs},
		"$or": []bson.M{
			{"lease_owner": bson.M{"$in": []any{nil, ""}}},
			{
				"lease_expires_at": bson.M{"$lte": now},
				"execution_state":  bson.M{"$ne": nil},
			},
		},
	}
	expiresAt := now.Add(leaseDur)
	// Promote queued → running atomically as part of the claim. The
	// dispatcher (POST /run, canvas WS, cron, webhook…) seeds new runs
	// with status=queued so the UI doesn't lie about runs sitting in
	// the queue. The first worker to land the findAndModify wins both
	// the lease AND the status flip in one round-trip — no separate
	// Update needed, and no window where status=running but
	// lease_owner is empty. Already-running runs (resumes, recovered
	// dead-worker runs) keep status=running through the same $set.
	// Aggregation-pipeline update so we can use `$ifNull` to set
	// started_at only on the FIRST claim. Re-claims (recovered
	// dead-worker runs) preserve the original run-start time so
	// duration math reflects total execution effort, not just the
	// most recent claim. Plain $set would clobber the original on
	// every claim. queued_at is never touched by ClaimLease — it
	// records dispatch time.
	update := bson.A{
		bson.M{"$set": bson.M{
			"lease_owner":      workerID,
			"lease_expires_at": expiresAt,
			"status":           string(workflow.RunStatusRunning),
			"started_at":       bson.M{"$ifNull": bson.A{"$started_at", now}},
		}},
	}
	// FIFO-ish: prefer the run with the oldest queued_at so long-
	// waiting runs don't get starved by a flood of fresh ones.
	opts := options.FindOneAndUpdate().
		SetUpsert(false).
		SetReturnDocument(options.After).
		SetSort(bson.D{{Key: "queued_at", Value: 1}})

	var out workflow.WorkflowRun
	err := r.col.FindOneAndUpdate(ctx, filter, update, opts).Decode(&out)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return workflow.WorkflowRun{}, false, nil
		}
		return workflow.WorkflowRun{}, false, err
	}
	return out, true, nil
}

// ExtendLease pushes lease_expires_at forward only when caller still
// holds the lease. Foreign extends return ErrLeaseNotHeld so the
// caller can abort cleanly. Implements the heartbeat side of the
// lease pattern: workers should call this every ~10s if leaseDur=30s.
func (r *WorkflowRunRepository) ExtendLease(ctx context.Context, runID, workerID string, leaseDur time.Duration) error {
	if runID == "" || workerID == "" {
		return errors.New("workflow run lease: runID and workerID required")
	}
	if leaseDur <= 0 {
		return errors.New("workflow run lease: leaseDur must be > 0")
	}
	now := time.Now().UTC()
	expiresAt := now.Add(leaseDur)
	res, err := r.col.UpdateOne(
		ctx,
		bson.M{"_id": runID, "lease_owner": workerID},
		bson.M{"$set": bson.M{"lease_expires_at": expiresAt}},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return workflow.ErrLeaseNotHeld
	}
	return nil
}

// ReleaseLease clears lease_owner + lease_expires_at only when caller
// still holds the lease. Foreign release is a no-op (returns nil) —
// defensive for double-release on cleanup paths.
func (r *WorkflowRunRepository) ReleaseLease(ctx context.Context, runID, workerID string) error {
	if runID == "" || workerID == "" {
		return errors.New("workflow run lease: runID and workerID required")
	}
	_, err := r.col.UpdateOne(
		ctx,
		bson.M{"_id": runID, "lease_owner": workerID},
		bson.M{"$unset": bson.M{"lease_owner": "", "lease_expires_at": ""}},
	)
	return err
}

// CheckpointExecutionState writes the BFS snapshot at a step boundary,
// gated on the caller still holding the lease (filter matches
// lease_owner == workerID). On lease loss returns ErrLeaseNotHeld so
// the caller aborts without clobbering state another worker now owns.
//
// `steps` and `status` are optional: pass nil/"" to skip those parts of
// the write. Bumps last_checkpoint_at on every successful update.
func (r *WorkflowRunRepository) CheckpointExecutionState(ctx context.Context, runID, workerID string, state workflow.ExecutionState, steps []workflow.StepResult, status workflow.RunStatus) error {
	if runID == "" || workerID == "" {
		return errors.New("workflow run checkpoint: runID and workerID required")
	}
	now := time.Now().UTC()
	set := bson.M{
		"execution_state":    state,
		"last_checkpoint_at": now,
	}
	if steps != nil {
		set["steps"] = steps
	}
	if status != "" {
		set["status"] = string(status)
	}
	res, err := r.col.UpdateOne(
		ctx,
		bson.M{"_id": runID, "lease_owner": workerID},
		bson.M{"$set": set},
	)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return workflow.ErrLeaseNotHeld
	}
	return nil
}

// ApplyApprovalDecision writes the user's decision into the run's
// pending execution-state gate, clears any held lease, flips status
// back to running, and stamps last_checkpoint_at. Caller follows up
// with a Redis wakeup publish so the next worker claims and applies
// the decision. Idempotent: re-applying on a run with no pending gate
// is a no-op (no error) — protects against duplicate clicks racing
// with worker apply.
func (r *WorkflowRunRepository) ApplyApprovalDecision(ctx context.Context, runID string, decision workflow.ApprovalDecision) error {
	if runID == "" {
		return errors.New("workflow run apply approval: runID required")
	}
	now := time.Now().UTC()
	res, err := r.col.UpdateOne(
		ctx,
		bson.M{"_id": runID, "execution_state.pending": bson.M{"$ne": nil}},
		bson.M{
			"$set": bson.M{
				"execution_state.pending.decision": decision,
				"status":                           string(workflow.RunStatusRunning),
				"last_checkpoint_at":               now,
			},
			"$unset": bson.M{
				"lease_owner":      "",
				"lease_expires_at": "",
			},
		},
	)
	if err != nil {
		return err
	}
	_ = res
	return nil
}
