package rediss

import (
	"context"
	"fmt"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/redis/go-redis/v9"
)

const (
	TradesChannel = "immaiwin:trades:detected"
	NewsChannel   = "immaiwin:news:articles"
)

type Client struct {
	rdb *redis.Client
}

func New(cfg config.RedisConfig) *Client {
	return &Client{
		rdb: redis.NewClient(&redis.Options{
			Addr:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
			Password: cfg.Password,
			DB:       cfg.DB,
		}),
	}
}

func (c *Client) Publish(ctx context.Context, channel string, payload []byte) error {
	return c.rdb.Publish(ctx, channel, payload).Err()
}

func (c *Client) Subscribe(ctx context.Context, channels ...string) *redis.PubSub {
	return c.rdb.Subscribe(ctx, channels...)
}

func (c *Client) Close() error {
	return c.rdb.Close()
}
