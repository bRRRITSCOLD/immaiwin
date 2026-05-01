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
		{Keys: bson.D{{Key: "workflow_id", Value: 1}, {Key: "started_at", Value: -1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "started_at", Value: -1}}},
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

// SweepAbandonedNonTerminal flips every non-terminal run older than
// `olderThan` into a terminal `error` state with the supplied reason.
// Used by the API boot path to recover from a hard crash / restart
// while runs were mid-flight: the in-process goroutines that held
// their state are gone, so the runs are unrecoverable. Returns the
// number of records flipped.
//
// `olderThan` is a safety window so a parallel API process that's
// legitimately running fresh work doesn't get its records stomped.
// Anything started AFTER (boot_time - olderThan) is assumed to belong
// to a still-alive process and skipped. Single-pod deployments can
// pass a near-zero window; multi-pod deployments need wider margins.
//
// Permanent fix is Phase 3 lease-based execution (Mongo-tracked
// run-owner leases that auto-renew); this method is the stop-gap.
func (r *WorkflowRunRepository) SweepAbandonedNonTerminal(ctx context.Context, reason string, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	filter := bson.M{
		"status": bson.M{"$in": []string{
			string(workflow.RunStatusRunning),
			string(workflow.RunStatusPaused),
			string(workflow.RunStatusPendingApproval),
		}},
		"started_at": bson.M{"$lt": cutoff},
	}
	now := time.Now().UTC()
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

// List returns the most recent runs for a workflow, newest first.
func (r *WorkflowRunRepository) List(ctx context.Context, workflowID string, limit int) ([]workflow.WorkflowRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "started_at", Value: -1}}).
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
		q["started_at"] = startedQ
	}

	opts := options.Find().
		SetSort(bson.D{{Key: "started_at", Value: -1}}).
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
			"started_at":  bson.M{"$gte": since},
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
		match["started_at"] = startedQ
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
	filter := bson.M{
		"status": bson.M{"$in": statusStrs},
		"$or": []bson.M{
			{"lease_owner": bson.M{"$in": []any{nil, ""}}},
			{"lease_expires_at": bson.M{"$lte": now}},
			{"lease_expires_at": bson.M{"$exists": false}},
		},
	}
	expiresAt := now.Add(leaseDur)
	update := bson.M{
		"$set": bson.M{
			"lease_owner":      workerID,
			"lease_expires_at": expiresAt,
		},
	}
	// FIFO-ish: prefer the run with the oldest started_at so long-waiting
	// runs don't get starved by a flood of fresh ones.
	opts := options.FindOneAndUpdate().
		SetUpsert(false).
		SetReturnDocument(options.After).
		SetSort(bson.D{{Key: "started_at", Value: 1}})

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
