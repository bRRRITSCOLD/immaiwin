package mongodb

import (
	"context"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type Client struct {
	client *mongo.Client
	db     *mongo.Database
}

func New(ctx context.Context, cfg config.MongoDBConfig) (*Client, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(cfg.URI))
	if err != nil {
		return nil, err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}

	return &Client{
		client: client,
		db:     client.Database(cfg.Database),
	}, nil
}

func (c *Client) DB() *mongo.Database {
	return c.db
}

func (c *Client) Disconnect(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}
