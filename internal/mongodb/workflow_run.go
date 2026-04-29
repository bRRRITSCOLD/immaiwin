package mongodb

import (
	"context"
	"errors"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
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

// AppendTrace atomically pushes a trace event to a run's agent_traces map.
// Mongo dot-notation on a map field: agent_traces.<nodeID>
func (r *WorkflowRunRepository) AppendTrace(ctx context.Context, runID, nodeID string, event workflow.TraceEvent) error {
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": runID},
		bson.M{"$push": bson.M{"agent_traces." + nodeID: event}},
	)
	return err
}
