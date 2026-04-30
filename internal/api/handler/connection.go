package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/auth"
	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/bRRRITSCOLD/burrow/internal/workflow"
	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// ConnectionStore is the persistence interface for workflow connections.
// Adds tenant-scoped variants alongside the unscoped versions used by
// internal callers (connection resolver, workers).
type ConnectionStore interface {
	List(ctx context.Context) ([]workflow.Connection, error)
	GetByID(ctx context.Context, id string) (workflow.Connection, error)
	ListForTenant(ctx context.Context, tenantID string) ([]workflow.Connection, error)
	GetByIDForTenant(ctx context.Context, id, tenantID string) (workflow.Connection, error)
	Upsert(ctx context.Context, conn workflow.Connection) (workflow.Connection, error)
	Delete(ctx context.Context, id string) error
}

// ConnectionInvalidator drops cached resolved clients for a connection ID,
// forcing the next workflow run to rebuild from current stored config.
type ConnectionInvalidator interface {
	Invalidate(connectionID string)
}

// ListConnections returns connections scoped to the caller's tenant.
// Unauthed legacy fallback returns all (Phase G removes).
func ListConnections(store ConnectionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var conns []workflow.Connection
		var err error
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			conns, err = store.ListForTenant(c.Request.Context(), tenantID)
		} else {
			conns, err = store.List(c.Request.Context())
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if conns == nil {
			conns = []workflow.Connection{}
		}
		c.JSON(http.StatusOK, conns)
	}
}

// UpsertConnection creates or updates a connection.
// invalidator is optional; when non-nil its cache is dropped after a
// successful save so live workflow runs pick up the new config.
func UpsertConnection(store ConnectionStore, invalidator ConnectionInvalidator) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}

		var conn workflow.Connection
		if err := c.ShouldBindJSON(&conn); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		conn.ID = id

		if conn.Name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
			return
		}
		validTypes := map[workflow.ConnectionType]bool{
			workflow.ConnectionTypeMongoDB:   true,
			workflow.ConnectionTypeRedis:     true,
			workflow.ConnectionTypeRabbitMQ:  true,
			workflow.ConnectionTypeAnthropic: true,
			workflow.ConnectionTypeOpenAI:    true,
			workflow.ConnectionTypeOllama:    true,
		}
		if !validTypes[conn.Type] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported connection type"})
			return
		}

		// Validate required config fields
		switch conn.Type {
		case workflow.ConnectionTypeMongoDB:
			if conn.Config["uri"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.uri is required for mongodb"})
				return
			}
			if conn.Config["database"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.database is required for mongodb"})
				return
			}
		case workflow.ConnectionTypeRedis:
			if conn.Config["host"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.host is required for redis"})
				return
			}
		case workflow.ConnectionTypeRabbitMQ:
			if conn.Config["host"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.host is required for rabbitmq"})
				return
			}
		case workflow.ConnectionTypeAnthropic:
			if conn.Config["api_key"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.api_key is required for anthropic"})
				return
			}
		case workflow.ConnectionTypeOpenAI:
			if conn.Config["api_key"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.api_key is required for openai"})
				return
			}
		case workflow.ConnectionTypeOllama:
			// No required config — endpoint defaults to http://localhost:11434.
		}

		// Tenant scoping: stamp the active tenant on new connections,
		// verify ownership on updates. Cross-tenant takeover blocked.
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			existing, gerr := store.GetByIDForTenant(c.Request.Context(), id, tenantID)
			if gerr == nil {
				conn.TenantID = existing.TenantID
				conn.CreatedAt = existing.CreatedAt
			} else {
				if _, anyErr := store.GetByID(c.Request.Context(), id); anyErr == nil {
					c.JSON(http.StatusForbidden, gin.H{"error": "connection exists in another tenant"})
					return
				}
				conn.TenantID = tenantID
			}
		}

		saved, err := store.Upsert(c.Request.Context(), conn)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if invalidator != nil {
			invalidator.Invalidate(saved.ID)
		}
		c.JSON(http.StatusOK, saved)
	}
}

// DeleteConnection removes the connection with the given ID. Tenant-
// scoped when authed: foreign-tenant connections return 404.
func DeleteConnection(store ConnectionStore, invalidator ConnectionInvalidator) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if tenantID, ok := auth.TenantFromCtx(c.Request.Context()); ok {
			if _, gerr := store.GetByIDForTenant(c.Request.Context(), id, tenantID); gerr != nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
		}
		if err := store.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if invalidator != nil {
			invalidator.Invalidate(id)
		}
		c.Status(http.StatusNoContent)
	}
}

// TestConnection accepts inline config in the request body and pings the target.
// No save — purely ephemeral connectivity test.
func TestConnection(db *mongo.Database) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Type   workflow.ConnectionType `json:"type"`
			Config map[string]string      `json:"config"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()

		switch req.Type {
		case workflow.ConnectionTypeMongoDB:
			if err := testMongoDB(ctx, req.Config); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeRedis:
			if err := testRedis(ctx, req.Config); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeRabbitMQ:
			if err := testRabbitMQ(req.Config); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeAnthropic:
			if err := testAnthropic(ctx, req.Config); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeOpenAI:
			if err := testOpenAI(ctx, req.Config); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeOllama:
			if err := testOllama(ctx, req.Config); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported connection type"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func testMongoDB(ctx context.Context, cfg map[string]string) error {
	client, err := mongo.Connect(workflow.BuildMongoOpts(cfg))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect(ctx) //nolint:errcheck
	if err := client.Ping(ctx, nil); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

func testRedis(ctx context.Context, cfg map[string]string) error {
	rdb := redis.NewClient(workflow.BuildRedisOpts(cfg))
	defer rdb.Close() //nolint:errcheck
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

func testRabbitMQ(cfg map[string]string) error {
	url := workflow.BuildRabbitMQURL(cfg)
	conn, err := amqp.Dial(url)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	return nil
}

func testAnthropic(_ context.Context, cfg map[string]string) error {
	// llm.Build validates required fields (api_key, etc.) without calling the
	// remote API. A live ping would consume tokens for every Test click.
	if _, err := llm.Build(string(workflow.ConnectionTypeAnthropic), cfg); err != nil {
		return err
	}
	return nil
}

func testOpenAI(_ context.Context, cfg map[string]string) error {
	// Same approach as testAnthropic — factory validates required fields
	// without burning tokens on a live call.
	if _, err := llm.Build(string(workflow.ConnectionTypeOpenAI), cfg); err != nil {
		return err
	}
	return nil
}

func testOllama(ctx context.Context, cfg map[string]string) error {
	// Ollama has no required config so factory always succeeds; do a live
	// HEAD on the endpoint instead so a misconfigured URL surfaces here
	// instead of at the first agent run. Local + cheap (no model load).
	if _, err := llm.Build(string(workflow.ConnectionTypeOllama), cfg); err != nil {
		return err
	}
	endpoint := cfg["endpoint"]
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	endpoint = strings.TrimRight(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("ping ollama: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ollama responded %d", resp.StatusCode)
	}
	return nil
}

