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

// WorkflowRepository persists workflow graphs in MongoDB.
type WorkflowRepository struct {
	col *mongo.Collection
}

func NewWorkflowRepository(ctx context.Context, db *mongo.Database) (*WorkflowRepository, error) {
	col := db.Collection("workflows")
	_, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "name", Value: 1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	return &WorkflowRepository{col: col}, nil
}

// scopedFilter folds a tenant scope into a Mongo filter. Empty tenant
// (e.g. trusted internal callers like cron worker which scans every
// workflow) returns the filter unchanged.
func scopedFilter(base bson.M, tenantID string) bson.M {
	if tenantID == "" {
		return base
	}
	out := bson.M{}
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out
}

// List returns every workflow without tenant filter. Used by workers
// that scan across the whole platform (cron, webhook lookup, ws-client
// dispatch, etc.). Tenant-scoped UI requests use ListForTenant.
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

// ListForTenant scopes List to a single tenant. Empty tenantID
// returns no rows (defensive — caller must explicitly pass an empty
// string only when they intended unscoped, which is what List is
// for).
func (r *WorkflowRepository) ListForTenant(ctx context.Context, tenantID string) ([]workflow.Workflow, error) {
	cur, err := r.col.Find(ctx, scopedFilter(bson.M{}, tenantID))
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

// GetByID returns the workflow with the given string ID, no scope.
// Used by workers + the executor's connection resolver for cross-
// tenant tools (skill catalogs, etc.). UI / per-user requests use
// GetByIDForTenant.
func (r *WorkflowRepository) GetByID(ctx context.Context, id string) (workflow.Workflow, error) {
	var wf workflow.Workflow
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&wf)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return workflow.Workflow{}, mongo.ErrNoDocuments
	}
	return wf, err
}

// GetByIDForTenant returns the workflow only when its tenant matches.
// Foreign-tenant lookups return ErrNoDocuments (same shape as missing
// — callers can't probe existence cross-tenant).
func (r *WorkflowRepository) GetByIDForTenant(ctx context.Context, id, tenantID string) (workflow.Workflow, error) {
	var wf workflow.Workflow
	err := r.col.FindOne(ctx, scopedFilter(bson.M{"_id": id}, tenantID)).Decode(&wf)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return workflow.Workflow{}, mongo.ErrNoDocuments
	}
	return wf, err
}

// Upsert saves or updates a workflow by its ID. Returns the post-write
// document so callers (and the UI) see the server-stamped Version.
//
// Version semantics: Mongo `$inc: {version: 1}` runs on every write.
// Against a missing field (insert path) the operator creates it at 1;
// against an existing doc it bumps by 1. Server-controlled — any
// `version` value the client sent on the wire is ignored, the
// `$set` whitelist below intentionally omits it.
func (r *WorkflowRepository) Upsert(ctx context.Context, wf workflow.Workflow) (workflow.Workflow, error) {
	now := time.Now().UTC()
	if wf.CreatedAt.IsZero() {
		wf.CreatedAt = now
	}
	wf.UpdatedAt = now

	// $set explicit-whitelists fields so unrelated docs don't pick up
	// schema drift. Whenever you add a Workflow field that should round-
	// trip via Upsert, add it here too — silent-drop bugs from this
	// list have bitten us twice. `version` is intentionally excluded:
	// it is server-controlled via `$inc` below.
	setFields := bson.M{
		"tenant_id":  wf.TenantID,
		"name":       wf.Name,
		"params":     wf.Params,
		"nodes":      wf.Nodes,
		"edges":      wf.Edges,
		"updated_at": wf.UpdatedAt,
		// `enabled` rides through every Upsert so an existing doc
		// can't silently drop the field (next PATCH would compare
		// against a zero-value `false` and short-circuit). The Go
		// Workflow.UnmarshalJSON normalises an absent JSON `enabled`
		// to true so older API clients keep workflows enabled.
		"enabled": wf.Enabled,
	}
	update := bson.M{
		"$set":         setFields,
		"$inc":         bson.M{"version": 1},
		"$setOnInsert": bson.M{"created_at": wf.CreatedAt},
	}
	unsetFields := bson.M{}
	// Disabled metadata travels via SetEnabled only; Upsert clears
	// stale fields if the caller passed a Workflow with them empty.
	if wf.DisabledAt != nil {
		setFields["disabled_at"] = wf.DisabledAt
	} else {
		unsetFields["disabled_at"] = ""
	}
	if wf.DisabledReason != "" {
		setFields["disabled_reason"] = wf.DisabledReason
	} else {
		unsetFields["disabled_reason"] = ""
	}
	// Pointer field — when nil (caps cleared) we want $unset rather than
	// $set: nil so existing docs don't carry a stale value. Mongo doesn't
	// allow $set + $unset on the same field in one update, so branch.
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
	// ApprovalChannel — same nil-vs-set treatment as cost_limits. Empty
	// channel cleared from existing docs so the dispatcher can't resurrect
	// stale routing after the user removes the field.
	if wf.ApprovalChannel != nil {
		setFields["approval_channel"] = wf.ApprovalChannel
	} else {
		unsetFields["approval_channel"] = ""
	}
	if len(unsetFields) > 0 {
		update["$unset"] = unsetFields
	}

	var saved workflow.Workflow
	err := r.col.FindOneAndUpdate(
		ctx,
		bson.M{"_id": wf.ID},
		update,
		options.FindOneAndUpdate().
			SetUpsert(true).
			SetReturnDocument(options.After),
	).Decode(&saved)
	if err != nil {
		return wf, err
	}
	return saved, nil
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

// SetEnabled flips the trigger-routing gate on a workflow without
// touching the rest of the doc. enabled=false also writes
// disabled_at + disabled_reason so the UI + audit log can surface
// who/when/why. enabled=true clears both. Returns ErrNoDocuments if
// the workflow is gone.
func (r *WorkflowRepository) SetEnabled(ctx context.Context, id string, enabled bool, reason string) (workflow.Workflow, error) {
	now := time.Now().UTC()
	setDoc := bson.M{"enabled": enabled, "updated_at": now}
	update := bson.M{"$set": setDoc}
	if enabled {
		update["$unset"] = bson.M{"disabled_at": "", "disabled_reason": ""}
	} else {
		setDoc["disabled_at"] = now
		setDoc["disabled_reason"] = reason
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var wf workflow.Workflow
	if err := r.col.FindOneAndUpdate(ctx, bson.M{"_id": id}, update, opts).Decode(&wf); err != nil {
		return workflow.Workflow{}, err
	}
	return wf, nil
}

// SetName rewrites just the display name of a workflow, leaving
// nodes / edges / params / version untouched. Used by the rename
// affordance in the workflow sidebar so a focused rename doesn't
// round-trip the full graph (and doesn't bump the schema version
// the way Upsert would). Returns ErrNoDocuments when the workflow
// is gone.
func (r *WorkflowRepository) SetName(ctx context.Context, id, name string) (workflow.Workflow, error) {
	update := bson.M{"$set": bson.M{
		"name":       name,
		"updated_at": time.Now().UTC(),
	}}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var wf workflow.Workflow
	if err := r.col.FindOneAndUpdate(ctx, bson.M{"_id": id}, update, opts).Decode(&wf); err != nil {
		return workflow.Workflow{}, err
	}
	return wf, nil
}

// BackfillEnabled sets enabled=true on every workflow document that
// was inserted before the field existed (no `enabled` key). Idempotent
// — re-running is a no-op. Called from cmd/api boot so existing
// deployments don't have every workflow appear disabled after upgrade.
func (r *WorkflowRepository) BackfillEnabled(ctx context.Context) (int64, error) {
	res, err := r.col.UpdateMany(ctx,
		bson.M{"enabled": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"enabled": true}},
	)
	if err != nil {
		return 0, err
	}
	return res.ModifiedCount, nil
}
