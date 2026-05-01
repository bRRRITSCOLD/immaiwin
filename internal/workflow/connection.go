package workflow

import (
	"context"
	"time"
)

// ConnectionType identifies the kind of external service a connection targets.
type ConnectionType string

const (
	ConnectionTypeMongoDB   ConnectionType = "mongodb"
	ConnectionTypeRedis     ConnectionType = "redis"
	ConnectionTypeRabbitMQ  ConnectionType = "rabbitmq"
	ConnectionTypeAnthropic ConnectionType = "anthropic"
	ConnectionTypeOpenAI    ConnectionType = "openai"
	ConnectionTypeOllama    ConnectionType = "ollama"
	// ConnectionTypeSlack carries an `xoxb-*` Slack bot token plus an
	// optional `default_channel` ID. Used by the Stage-2 OOB approval
	// notifier when the workflow's `ApprovalChannel.Type == "slack_bot"`.
	// Token gets encrypted at rest by the existing connection-encryption
	// layer (AES-256-GCM via ENCRYPTION_KEY).
	ConnectionTypeSlack ConnectionType = "slack"
)

// Connection is a named, reusable configuration for an external service.
// Nodes can reference a connection by ID; missing ID falls back to the default env-var connection.
type Connection struct {
	ID        string            `bson:"_id,omitempty" json:"id"`
	TenantID  string            `bson:"tenant_id"     json:"tenant_id"`
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
