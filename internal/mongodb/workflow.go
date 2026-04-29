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

// WorkflowRepository persists workflow graphs in MongoDB.
type WorkflowRepository struct {
	col *mongo.Collection
}

func NewWorkflowRepository(ctx context.Context, db *mongo.Database) (*WorkflowRepository, error) {
	col := db.Collection("workflows")
	_, err := col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}},
	})
	if err != nil {
		return nil, err
	}
	return &WorkflowRepository{col: col}, nil
}

// List returns all stored workflows.
func (r *WorkflowRepository) List(ctx context.Context) ([]workflow.Workflow, error) {
	cur, err := r.col.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx) //nolint:errcheck
	var results []workflow.Workflow
	if err := cur.All(ctx, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetByID returns the workflow with the given string ID.
func (r *WorkflowRepository) GetByID(ctx context.Context, id string) (workflow.Workflow, error) {
	var wf workflow.Workflow
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&wf)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return workflow.Workflow{}, mongo.ErrNoDocuments
	}
	return wf, err
}

// Upsert saves or updates a workflow by its ID.
// If CreatedAt is zero it is set to now on first insert.
func (r *WorkflowRepository) Upsert(ctx context.Context, wf workflow.Workflow) (workflow.Workflow, error) {
	now := time.Now().UTC()
	if wf.CreatedAt.IsZero() {
		wf.CreatedAt = now
	}
	wf.UpdatedAt = now

	// $set explicit-whitelists fields so unrelated docs don't pick up
	// schema drift. Whenever you add a Workflow field that should round-
	// trip via Upsert, add it here too — silent-drop bugs from this
	// list have bitten us twice.
	setFields := bson.M{
		"name":       wf.Name,
		"params":     wf.Params,
		"nodes":      wf.Nodes,
		"edges":      wf.Edges,
		"updated_at": wf.UpdatedAt,
	}
	// Pointer field — when nil (caps cleared) we want $unset rather than
	// $set: nil so existing docs don't carry a stale value. Mongo doesn't
	// allow $set + $unset on the same field in one update, so branch.
	update := bson.M{
		"$set":         setFields,
		"$setOnInsert": bson.M{"created_at": wf.CreatedAt},
	}
	unsetFields := bson.M{}
	if wf.CostLimits != nil {
		setFields["cost_limits"] = wf.CostLimits
	} else {
		unsetFields["cost_limits"] = ""
	}
	// ParamsSchema persists too. Empty slice → $unset so legacy docs
	// don't accumulate empty arrays after a clear.
	if len(wf.ParamsSchema) > 0 {
		setFields["params_schema"] = wf.ParamsSchema
	} else {
		unsetFields["params_schema"] = ""
	}
	if len(unsetFields) > 0 {
		update["$unset"] = unsetFields
	}

	_, err := r.col.UpdateOne(
		ctx,
		bson.M{"_id": wf.ID},
		update,
		options.UpdateOne().SetUpsert(true),
	)
	return wf, err
}

// Delete removes the workflow with the given ID.
func (r *WorkflowRepository) Delete(ctx context.Context, id string) error {
	_, err := r.col.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// FindByName returns the first workflow matching name.
func (r *WorkflowRepository) FindByName(ctx context.Context, name string) (workflow.Workflow, error) {
	var wf workflow.Workflow
	err := r.col.FindOne(ctx, bson.M{"name": name}).Decode(&wf)
	return wf, err
}
