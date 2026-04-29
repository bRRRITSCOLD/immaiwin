package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/oklog/ulid/v2"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// EvalRepository persists evals + their runs. Two collections:
//   - `evals`            — Eval definitions
//   - `eval_runs`        — EvalRun executions, indexed by eval_id + started_at
//
// Compile-time interface check at the bottom keeps `workflow.EvalStore`
// in sync with this implementation.
type EvalRepository struct {
	evalCol    *mongo.Collection
	evalRunCol *mongo.Collection
}

// NewEvalRepository constructs the repo + ensures indexes.
func NewEvalRepository(ctx context.Context, db *mongo.Database) (*EvalRepository, error) {
	evals := db.Collection("evals")
	runs := db.Collection("eval_runs")

	if _, err := evals.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "workflow_id", Value: 1}, {Key: "name", Value: 1}}},
	}); err != nil {
		return nil, err
	}
	if _, err := runs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "eval_id", Value: 1}, {Key: "started_at", Value: -1}}},
		{Keys: bson.D{{Key: "workflow_id", Value: 1}, {Key: "started_at", Value: -1}}},
	}); err != nil {
		return nil, err
	}

	return &EvalRepository{evalCol: evals, evalRunCol: runs}, nil
}

// --- Eval CRUD ---

// UpsertEval saves or updates an eval definition. Generates a ULID when
// the input has no ID; updates the timestamps; bumps the Version. The
// version increments on every Save (rename, body edit, anything) so
// EvalRun.EvalVersion + EvalSnapshot can pinpoint the exact config that
// produced any historical run.
func (r *EvalRepository) UpsertEval(ctx context.Context, eval workflow.Eval) (workflow.Eval, error) {
	now := time.Now().UTC()
	if eval.ID == "" {
		eval.ID = ulid.Make().String()
	}
	eval.UpdatedAt = now

	// Read the current version (if any) and bump. Mongo doesn't have a
	// generic atomic "$inc + return" unless we use FindOneAndUpdate;
	// keep this simple — single-writer assumption holds for the UI's
	// sequential save flow.
	var existing workflow.Eval
	if err := r.evalCol.FindOne(ctx, bson.M{"_id": eval.ID}).Decode(&existing); err == nil {
		eval.Version = existing.Version + 1
		if eval.CreatedAt.IsZero() {
			eval.CreatedAt = existing.CreatedAt
		}
	} else {
		eval.Version = 1
	}
	// Backstop: first-save path (no existing doc + caller-supplied ID,
	// e.g. UI generates a UUID before PUT) leaves CreatedAt zero. Always
	// fill if still empty so eval list rendering doesn't show year-0001.
	if eval.CreatedAt.IsZero() {
		eval.CreatedAt = now
	}

	_, err := r.evalCol.UpdateOne(ctx,
		bson.M{"_id": eval.ID},
		bson.M{"$set": eval},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return workflow.Eval{}, fmt.Errorf("mongodb/evals: upsert: %w", err)
	}
	return eval, nil
}

// GetEval fetches an eval definition by ID.
func (r *EvalRepository) GetEval(ctx context.Context, id string) (workflow.Eval, error) {
	var eval workflow.Eval
	err := r.evalCol.FindOne(ctx, bson.M{"_id": id}).Decode(&eval)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return workflow.Eval{}, errors.New("eval not found")
	}
	return eval, err
}

// ListEvals returns evals filtered by workflow (or all when workflowID is
// empty), sorted by name for stable display order.
func (r *EvalRepository) ListEvals(ctx context.Context, workflowID string) ([]workflow.Eval, error) {
	filter := bson.M{}
	if workflowID != "" {
		filter["workflow_id"] = workflowID
	}
	cur, err := r.evalCol.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck

	var out []workflow.Eval
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteEval removes an eval definition.
func (r *EvalRepository) DeleteEval(ctx context.Context, id string) error {
	_, err := r.evalCol.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// --- EvalRun persistence ---

// CreateEvalRun inserts a new run record. Caller must populate the ID
// (ULID); we don't generate here so the runner can correlate the
// pre-create record with the post-execution update.
func (r *EvalRepository) CreateEvalRun(ctx context.Context, run workflow.EvalRun) (workflow.EvalRun, error) {
	if run.ID == "" {
		return workflow.EvalRun{}, errors.New("mongodb/eval_runs: ID required")
	}
	if _, err := r.evalRunCol.InsertOne(ctx, run); err != nil {
		return workflow.EvalRun{}, err
	}
	return run, nil
}

// UpdateEvalRun replaces the full document — used when the runner
// finalises pass/fail counts + per-case results.
func (r *EvalRepository) UpdateEvalRun(ctx context.Context, run workflow.EvalRun) error {
	_, err := r.evalRunCol.ReplaceOne(ctx, bson.M{"_id": run.ID}, run)
	return err
}

// GetEvalRun fetches a single run by ID.
func (r *EvalRepository) GetEvalRun(ctx context.Context, id string) (workflow.EvalRun, error) {
	var run workflow.EvalRun
	err := r.evalRunCol.FindOne(ctx, bson.M{"_id": id}).Decode(&run)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return workflow.EvalRun{}, errors.New("eval run not found")
	}
	return run, err
}

// ListEvalRuns returns the most-recent runs for an eval, newest first.
// Limit defaults to 50.
func (r *EvalRepository) ListEvalRuns(ctx context.Context, evalID string, limit int) ([]workflow.EvalRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cur, err := r.evalRunCol.Find(ctx,
		bson.M{"eval_id": evalID},
		options.Find().
			SetSort(bson.D{{Key: "started_at", Value: -1}}).
			SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck

	var out []workflow.EvalRun
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Compile-time interface check.
var _ workflow.EvalStore = (*EvalRepository)(nil)
