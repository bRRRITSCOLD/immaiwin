// Worker health store. Each registered worker writes a heartbeat doc
// keyed by worker name; the api process reads them to expose
// /api/v1/workers/health for ops dashboards + alerting.
//
// Why one doc per worker (not per instance): with concurrency=1 today
// a 1:1 name→doc model is plenty. If we ever scale a single worker
// kind to N instances, the schema will need an `instance` discriminator
// — but premature optimisation now.
//
// Update semantics: heartbeats use upsert so the first beat creates
// the row, subsequent beats overwrite the heartbeat fields without
// touching `started_at` (which is set by the registry when Run starts
// and serves as "uptime since").

package mongodb

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// WorkerHealth is the persisted heartbeat row.
type WorkerHealth struct {
	Name          string     `bson:"_id"                       json:"name"`
	StartedAt     time.Time  `bson:"started_at"                json:"started_at"`
	LastHeartbeat time.Time  `bson:"last_heartbeat"            json:"last_heartbeat"`
	TickCount     int64      `bson:"tick_count"                json:"tick_count"`
	ErrorCount    int64      `bson:"error_count,omitempty"     json:"error_count,omitempty"`
	LastError     string     `bson:"last_error,omitempty"      json:"last_error,omitempty"`
	LastErrorAt   *time.Time `bson:"last_error_at,omitempty"   json:"last_error_at,omitempty"`
	StoppedAt     *time.Time `bson:"stopped_at,omitempty"      json:"stopped_at,omitempty"`
	// Status snapshot — denormalised so the UI doesn't compute it from
	// the timestamps. "running" while heartbeats are fresh, "stopped"
	// after Run() returns cleanly, "errored" after fatal Run() error.
	Status string `bson:"status" json:"status"`
}

// WorkerHealthRepository persists per-worker heartbeats.
type WorkerHealthRepository struct {
	col *mongo.Collection
}

func NewWorkerHealthRepository(ctx context.Context, db *mongo.Database) (*WorkerHealthRepository, error) {
	r := &WorkerHealthRepository{col: db.Collection("worker_health")}
	// Index on last_heartbeat so the api's /workers/health endpoint
	// can sort recent-first cheaply, and so an alert query like
	// "find workers w/ no heartbeat in 5min" stays fast.
	_, err := r.col.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "last_heartbeat", Value: -1}},
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// MarkStarted records a fresh start of the named worker. Resets
// status to "running", stamps started_at, clears stopped_at + last_error.
// Called once at Run() entry.
func (r *WorkerHealthRepository) MarkStarted(ctx context.Context, name string) error {
	now := time.Now().UTC()
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{
			"$set": bson.M{
				"started_at":     now,
				"last_heartbeat": now,
				"status":         "running",
			},
			"$unset": bson.M{
				"stopped_at":    "",
				"last_error":    "",
				"last_error_at": "",
			},
			"$setOnInsert": bson.M{
				"tick_count":  int64(0),
				"error_count": int64(0),
			},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// Beat advances the heartbeat + increments tick_count. Cheap — one
// mongo update. Called by workers' main loop or by the registry
// wrapper at a fixed cadence (~30s) regardless of internal tick rate.
func (r *WorkerHealthRepository) Beat(ctx context.Context, name string) error {
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{
			"$set": bson.M{
				"last_heartbeat": time.Now().UTC(),
				"status":         "running",
			},
			"$inc": bson.M{"tick_count": 1},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// MarkStopped records a clean exit (Run returned nil). Status flips
// to "stopped" so dashboards can distinguish a deliberate shutdown
// from a stale heartbeat (unresponsive worker).
func (r *WorkerHealthRepository) MarkStopped(ctx context.Context, name string) error {
	now := time.Now().UTC()
	_, err := r.col.UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{"$set": bson.M{
			"stopped_at": now,
			"status":     "stopped",
		}},
	)
	return err
}

// RecordError captures a worker's fatal error. Status flips to
// "errored" + last_error / last_error_at populated. Increments
// error_count so dashboards can track flap rate.
func (r *WorkerHealthRepository) RecordError(ctx context.Context, name string, err error) error {
	if err == nil {
		return nil
	}
	now := time.Now().UTC()
	_, uerr := r.col.UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{
			"$set": bson.M{
				"last_error":    err.Error(),
				"last_error_at": now,
				"status":        "errored",
			},
			"$inc": bson.M{"error_count": 1},
		},
		options.UpdateOne().SetUpsert(true),
	)
	return uerr
}

// List returns all worker health rows, sorted by last_heartbeat desc.
// Empty list when no workers have ever beat.
func (r *WorkerHealthRepository) List(ctx context.Context) ([]WorkerHealth, error) {
	cur, err := r.col.Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "last_heartbeat", Value: -1}}),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var out []WorkerHealth
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
