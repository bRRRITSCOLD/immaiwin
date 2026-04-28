package rediss

import (
	"context"
	"fmt"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/redis/go-redis/v9"
)

const (
	TradesChannel  = "immaiwin:trades:detected"
	NewsChannel    = "immaiwin:news:articles"
	OptionsChannel = "immaiwin:options:unusual"
	FuturesChannel = "immaiwin:futures:trades"
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

// --- pub/sub (producer side only) ---

func (c *Client) Publish(ctx context.Context, channel string, payload []byte) error {
	return c.rdb.Publish(ctx, channel, payload).Err()
}

// Subscribe is retained for the news Broadcaster + future trigger nodes.
// It is intentionally NOT part of the workflow.RedisClient interface.
func (c *Client) Subscribe(ctx context.Context, channels ...string) *redis.PubSub {
	return c.rdb.Subscribe(ctx, channels...)
}

// --- strings ---

func (c *Client) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *Client) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Del(ctx, keys...).Result()
}

func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

func (c *Client) Decr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Decr(ctx, key).Result()
}

func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return c.rdb.Expire(ctx, key, ttl).Result()
}

func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.rdb.TTL(ctx, key).Result()
}

func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Exists(ctx, keys...).Result()
}

func (c *Client) Keys(ctx context.Context, pattern string) ([]string, error) {
	return c.rdb.Keys(ctx, pattern).Result()
}

func (c *Client) MGet(ctx context.Context, keys ...string) ([]any, error) {
	return c.rdb.MGet(ctx, keys...).Result()
}

func (c *Client) MSet(ctx context.Context, pairs map[string]string) error {
	args := make([]any, 0, len(pairs)*2)
	for k, v := range pairs {
		args = append(args, k, v)
	}
	return c.rdb.MSet(ctx, args...).Err()
}

// --- hashes ---

func (c *Client) HGet(ctx context.Context, key, field string) (string, error) {
	v, err := c.rdb.HGet(ctx, key, field).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *Client) HSet(ctx context.Context, key string, fields map[string]string) (int64, error) {
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	return c.rdb.HSet(ctx, key, args...).Result()
}

func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

func (c *Client) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return c.rdb.HDel(ctx, key, fields...).Result()
}

// --- lists ---

func (c *Client) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.LPush(ctx, key, values...).Result()
}

func (c *Client) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.RPush(ctx, key, values...).Result()
}

func (c *Client) LPop(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.LPop(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *Client) RPop(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.RPop(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *Client) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.LRange(ctx, key, start, stop).Result()
}

func (c *Client) LLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.LLen(ctx, key).Result()
}

// --- sets ---

func (c *Client) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SAdd(ctx, key, members...).Result()
}

func (c *Client) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SRem(ctx, key, members...).Result()
}

func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.rdb.SIsMember(ctx, key, member).Result()
}

// --- sorted sets ---

func (c *Client) ZAdd(ctx context.Context, key string, members map[string]float64) (int64, error) {
	z := make([]redis.Z, 0, len(members))
	for m, score := range members {
		z = append(z, redis.Z{Score: score, Member: m})
	}
	return c.rdb.ZAdd(ctx, key, z...).Result()
}

func (c *Client) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.ZRem(ctx, key, members...).Result()
}

func (c *Client) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.ZRange(ctx, key, start, stop).Result()
}

func (c *Client) ZScore(ctx context.Context, key, member string) (float64, error) {
	v, err := c.rdb.ZScore(ctx, key, member).Result()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (c *Client) ZIncrBy(ctx context.Context, key string, increment float64, member string) (float64, error) {
	return c.rdb.ZIncrBy(ctx, key, increment, member).Result()
}

// --- streams (producer + range) ---

func (c *Client) XAdd(ctx context.Context, stream string, values map[string]any) (string, error) {
	return c.rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Result()
}

func (c *Client) XRange(ctx context.Context, stream, start, stop string) ([]redis.XMessage, error) {
	return c.rdb.XRange(ctx, stream, start, stop).Result()
}

func (c *Client) XLen(ctx context.Context, stream string) (int64, error) {
	return c.rdb.XLen(ctx, stream).Result()
}

func (c *Client) Close() error {
	return c.rdb.Close()
}
