package handler

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/polymarket"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/workflow"
	"github.com/gin-gonic/gin"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// ConnectionStore is the persistence interface for workflow connections.
type ConnectionStore interface {
	List(ctx context.Context) ([]workflow.Connection, error)
	GetByID(ctx context.Context, id string) (workflow.Connection, error)
	Upsert(ctx context.Context, conn workflow.Connection) (workflow.Connection, error)
	Delete(ctx context.Context, id string) error
}

// ListConnections returns all stored connections.
func ListConnections(store ConnectionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		conns, err := store.List(c.Request.Context())
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
func UpsertConnection(store ConnectionStore) gin.HandlerFunc {
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
			workflow.ConnectionTypeMongoDB:    true,
			workflow.ConnectionTypeRedis:      true,
			workflow.ConnectionTypeRabbitMQ:   true,
			workflow.ConnectionTypePolymarket: true,
			workflow.ConnectionTypeSchwab:     true,
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
		case workflow.ConnectionTypePolymarket:
			// No required config — SDK falls back to env vars
		case workflow.ConnectionTypeSchwab:
			if conn.Config["client_id"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.client_id is required for schwab"})
				return
			}
			if conn.Config["client_secret"] == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "config.client_secret is required for schwab"})
				return
			}
		}

		saved, err := store.Upsert(c.Request.Context(), conn)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, saved)
	}
}

// DeleteConnection removes the connection with the given ID.
func DeleteConnection(store ConnectionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if err := store.Delete(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
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
		case workflow.ConnectionTypePolymarket:
			if err := testPolymarket(); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeSchwab:
			if err := testSchwab(ctx, req.Config, db); err != nil {
				c.JSON(http.StatusOK, gin.H{"ok": false, "error": err.Error()})
				return
			}
		case workflow.ConnectionTypeAnthropic:
			if err := testAnthropic(ctx, req.Config); err != nil {
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

func testSchwab(ctx context.Context, cfg map[string]string, db *mongo.Database) error {
	if cfg["client_id"] == "" || cfg["client_secret"] == "" {
		return fmt.Errorf("client_id and client_secret are required")
	}
	// For Schwab, "test" means checking if OAuth tokens exist (connection is authorized).
	// We use a placeholder connection ID since this is ephemeral.
	tm := workflow.BuildSchwabTokenManager(cfg, db, "test_ephemeral")
	if err := tm.Load(ctx); err != nil {
		return fmt.Errorf("load tokens: %w", err)
	}
	if !tm.IsAuthorized() {
		return fmt.Errorf("not authorized — complete OAuth flow first")
	}
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

func testPolymarket() error {
	client, err := polymarket.New(polymarket.ClientConfig{})
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	defer client.Close() //nolint:errcheck
	return nil
}
