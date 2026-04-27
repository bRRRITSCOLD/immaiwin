package workflow

import (
	"context"
	"time"
)

// ConnectionType identifies the kind of external service a connection targets.
type ConnectionType string

const (
	ConnectionTypeMongoDB    ConnectionType = "mongodb"
	ConnectionTypeRedis      ConnectionType = "redis"
	ConnectionTypeRabbitMQ   ConnectionType = "rabbitmq"
	ConnectionTypePolymarket ConnectionType = "polymarket"
	ConnectionTypeSchwab     ConnectionType = "schwab"
)

// Connection is a named, reusable configuration for an external service.
// Nodes can reference a connection by ID; missing ID falls back to the default env-var connection.
type Connection struct {
	ID        string            `bson:"_id,omitempty" json:"id"`
	Name      string            `bson:"name"          json:"name"`
	Type      ConnectionType    `bson:"type"          json:"type"`
	Config    map[string]string `bson:"config"        json:"config"`
	CreatedAt time.Time         `bson:"created_at"    json:"created_at"`
	UpdatedAt time.Time         `bson:"updated_at"    json:"updated_at"`
}

// ConnectionStore is the persistence interface for workflow connections.
type ConnectionStore interface {
	List(ctx context.Context) ([]Connection, error)
	GetByID(ctx context.Context, id string) (Connection, error)
	Upsert(ctx context.Context, conn Connection) (Connection, error)
	Delete(ctx context.Context, id string) error
}
