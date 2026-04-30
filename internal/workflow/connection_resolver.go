package workflow

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bRRRITSCOLD/burrow/internal/llm"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ConnectionResolver resolves connection IDs to live DB/Redis/LLM clients.
// Empty connection ID returns the default client (or an error for LLM,
// which has no default). Resolved clients are cached per ID.
type ConnectionResolver struct {
	store        ConnectionStore
	defaultDB    MongoClient
	defaultRedis RedisClient
	mu           sync.Mutex
	dbCache      map[string]resolvedDB
	redisCache   map[string]resolvedRedis
	llmCache     map[string]llm.Provider
}

type resolvedDB struct {
	client *mongo.Client
	mongo  MongoClient
}

type resolvedRedis struct {
	client *redis.Client
	redis  RedisClient
}

// mongoClientImpl wraps *mongo.Database to satisfy MongoClient — mirrors mongodb.MongoClient
// but lives here to avoid a circular import.
type mongoClientImpl struct {
	db *mongo.Database
}

func (c *mongoClientImpl) Find(ctx context.Context, collection string, filter bson.M, opts *options.FindOptionsBuilder) (*mongo.Cursor, error) {
	return c.db.Collection(collection).Find(ctx, filter, opts)
}

func (c *mongoClientImpl) FindOneAndUpdate(ctx context.Context, collection string, filter, update bson.M, opts *options.FindOneAndUpdateOptionsBuilder) (bson.M, error) {
	return decodeSingleResult(c.db.Collection(collection).FindOneAndUpdate(ctx, filter, update, opts))
}

func (c *mongoClientImpl) FindOneAndReplace(ctx context.Context, collection string, filter, replacement bson.M, opts *options.FindOneAndReplaceOptionsBuilder) (bson.M, error) {
	return decodeSingleResult(c.db.Collection(collection).FindOneAndReplace(ctx, filter, replacement, opts))
}

func (c *mongoClientImpl) InsertOne(ctx context.Context, collection string, doc bson.M) (any, error) {
	res, err := c.db.Collection(collection).InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	return res.InsertedID, nil
}

func (c *mongoClientImpl) InsertMany(ctx context.Context, collection string, docs []bson.M, opts *options.InsertManyOptionsBuilder) ([]any, error) {
	anyDocs := make([]any, len(docs))
	for i, d := range docs {
		anyDocs[i] = d
	}
	res, err := c.db.Collection(collection).InsertMany(ctx, anyDocs, opts)
	if err != nil {
		return nil, err
	}
	return res.InsertedIDs, nil
}

func (c *mongoClientImpl) UpdateMany(ctx context.Context, collection string, filter, update bson.M, opts *options.UpdateManyOptionsBuilder) (int64, int64, int64, error) {
	res, err := c.db.Collection(collection).UpdateMany(ctx, filter, update, opts)
	if err != nil {
		return 0, 0, 0, err
	}
	return res.MatchedCount, res.ModifiedCount, res.UpsertedCount, nil
}

func (c *mongoClientImpl) DeleteOne(ctx context.Context, collection string, filter bson.M) (int64, error) {
	res, err := c.db.Collection(collection).DeleteOne(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (c *mongoClientImpl) DeleteMany(ctx context.Context, collection string, filter bson.M) (int64, error) {
	res, err := c.db.Collection(collection).DeleteMany(ctx, filter)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (c *mongoClientImpl) Aggregate(ctx context.Context, collection string, pipeline []bson.M, opts *options.AggregateOptionsBuilder) (*mongo.Cursor, error) {
	return c.db.Collection(collection).Aggregate(ctx, pipeline, opts)
}

func (c *mongoClientImpl) CountDocuments(ctx context.Context, collection string, filter bson.M, opts *options.CountOptionsBuilder) (int64, error) {
	return c.db.Collection(collection).CountDocuments(ctx, filter, opts)
}

func (c *mongoClientImpl) Distinct(ctx context.Context, collection, field string, filter bson.M) ([]any, error) {
	res := c.db.Collection(collection).Distinct(ctx, field, filter)
	if err := res.Err(); err != nil {
		return nil, err
	}
	var values []any
	if err := res.Decode(&values); err != nil {
		return nil, err
	}
	return values, nil
}

// redisClientImpl wraps *redis.Client to satisfy RedisClient. Mirrors the
// surface in internal/rediss but lives here to avoid pulling in that package
// for the per-connection resolved case.
type redisClientImpl struct {
	rdb *redis.Client
}

func (c *redisClientImpl) Publish(ctx context.Context, channel string, payload []byte) error {
	return c.rdb.Publish(ctx, channel, payload).Err()
}

func (c *redisClientImpl) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *redisClientImpl) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

func (c *redisClientImpl) Del(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Del(ctx, keys...).Result()
}

func (c *redisClientImpl) Incr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Incr(ctx, key).Result()
}

func (c *redisClientImpl) Decr(ctx context.Context, key string) (int64, error) {
	return c.rdb.Decr(ctx, key).Result()
}

func (c *redisClientImpl) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return c.rdb.Expire(ctx, key, ttl).Result()
}

func (c *redisClientImpl) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.rdb.TTL(ctx, key).Result()
}

func (c *redisClientImpl) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Exists(ctx, keys...).Result()
}

func (c *redisClientImpl) Keys(ctx context.Context, pattern string) ([]string, error) {
	return c.rdb.Keys(ctx, pattern).Result()
}

func (c *redisClientImpl) MGet(ctx context.Context, keys ...string) ([]any, error) {
	return c.rdb.MGet(ctx, keys...).Result()
}

func (c *redisClientImpl) MSet(ctx context.Context, pairs map[string]string) error {
	args := make([]any, 0, len(pairs)*2)
	for k, v := range pairs {
		args = append(args, k, v)
	}
	return c.rdb.MSet(ctx, args...).Err()
}

func (c *redisClientImpl) HGet(ctx context.Context, key, field string) (string, error) {
	v, err := c.rdb.HGet(ctx, key, field).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *redisClientImpl) HSet(ctx context.Context, key string, fields map[string]string) (int64, error) {
	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}
	return c.rdb.HSet(ctx, key, args...).Result()
}

func (c *redisClientImpl) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

func (c *redisClientImpl) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return c.rdb.HDel(ctx, key, fields...).Result()
}

func (c *redisClientImpl) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.LPush(ctx, key, values...).Result()
}

func (c *redisClientImpl) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.rdb.RPush(ctx, key, values...).Result()
}

func (c *redisClientImpl) LPop(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.LPop(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *redisClientImpl) RPop(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.RPop(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c *redisClientImpl) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.LRange(ctx, key, start, stop).Result()
}

func (c *redisClientImpl) LLen(ctx context.Context, key string) (int64, error) {
	return c.rdb.LLen(ctx, key).Result()
}

func (c *redisClientImpl) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SAdd(ctx, key, members...).Result()
}

func (c *redisClientImpl) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.SRem(ctx, key, members...).Result()
}

func (c *redisClientImpl) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.rdb.SMembers(ctx, key).Result()
}

func (c *redisClientImpl) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.rdb.SIsMember(ctx, key, member).Result()
}

func (c *redisClientImpl) ZAdd(ctx context.Context, key string, members map[string]float64) (int64, error) {
	z := make([]redis.Z, 0, len(members))
	for m, score := range members {
		z = append(z, redis.Z{Score: score, Member: m})
	}
	return c.rdb.ZAdd(ctx, key, z...).Result()
}

func (c *redisClientImpl) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.rdb.ZRem(ctx, key, members...).Result()
}

func (c *redisClientImpl) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.rdb.ZRange(ctx, key, start, stop).Result()
}

func (c *redisClientImpl) ZScore(ctx context.Context, key, member string) (float64, error) {
	v, err := c.rdb.ZScore(ctx, key, member).Result()
	if err == redis.Nil {
		return 0, nil
	}
	return v, err
}

func (c *redisClientImpl) ZIncrBy(ctx context.Context, key string, increment float64, member string) (float64, error) {
	return c.rdb.ZIncrBy(ctx, key, increment, member).Result()
}

func (c *redisClientImpl) XAdd(ctx context.Context, stream string, values map[string]any) (string, error) {
	return c.rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: values}).Result()
}

func (c *redisClientImpl) XRange(ctx context.Context, stream, start, stop string) ([]redis.XMessage, error) {
	return c.rdb.XRange(ctx, stream, start, stop).Result()
}

func (c *redisClientImpl) XLen(ctx context.Context, stream string) (int64, error) {
	return c.rdb.XLen(ctx, stream).Result()
}

func decodeSingleResult(res *mongo.SingleResult) (bson.M, error) {
	if err := res.Err(); err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	var out bson.M
	if err := res.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func NewConnectionResolver(store ConnectionStore, defaultDB MongoClient, defaultRedis RedisClient) *ConnectionResolver {
	return &ConnectionResolver{
		store:        store,
		defaultDB:    defaultDB,
		defaultRedis: defaultRedis,
		dbCache:      make(map[string]resolvedDB),
		redisCache:   make(map[string]resolvedRedis),
		llmCache:     make(map[string]llm.Provider),
	}
}

// ResolveLLM returns an llm.Provider for the given connection ID.
// Unlike DB/Pub, LLM has no default — connectionID is required.
// Connection type must be registered with the llm package (e.g. anthropic).
func (r *ConnectionResolver) ResolveLLM(ctx context.Context, connectionID string) (llm.Provider, error) {
	if connectionID == "" {
		return nil, fmt.Errorf("resolve llm: connection_id required (no default LLM)")
	}

	r.mu.Lock()
	if cached, ok := r.llmCache[connectionID]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	conn, err := r.store.GetByID(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("resolve llm connection %q: %w", connectionID, err)
	}

	provider, err := llm.Build(string(conn.Type), conn.Config)
	if err != nil {
		return nil, fmt.Errorf("build llm provider %q (type=%s): %w", connectionID, conn.Type, err)
	}

	r.mu.Lock()
	r.llmCache[connectionID] = provider
	r.mu.Unlock()
	return provider, nil
}

// ResolveDB returns a MongoClient for the given connection ID.
// Empty ID returns the default.
func (r *ConnectionResolver) ResolveDB(ctx context.Context, connectionID string) (MongoClient, error) {
	if connectionID == "" {
		return r.defaultDB, nil
	}

	r.mu.Lock()
	if cached, ok := r.dbCache[connectionID]; ok {
		r.mu.Unlock()
		return cached.mongo, nil
	}
	r.mu.Unlock()

	conn, err := r.store.GetByID(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("resolve db connection %q: %w", connectionID, err)
	}
	if conn.Type != ConnectionTypeMongoDB {
		return nil, fmt.Errorf("connection %q is %s, expected mongodb", connectionID, conn.Type)
	}

	client, err := mongo.Connect(BuildMongoOpts(conn.Config))
	if err != nil {
		return nil, fmt.Errorf("connect mongodb %q: %w", connectionID, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("ping mongodb %q: %w", connectionID, err)
	}

	database := conn.Config["database"]
	if database == "" {
		database = "burrow"
	}
	mc := &mongoClientImpl{db: client.Database(database)}

	r.mu.Lock()
	r.dbCache[connectionID] = resolvedDB{client: client, mongo: mc}
	r.mu.Unlock()

	return mc, nil
}

// ResolveRedis returns a RedisClient for the given connection ID.
// Empty ID returns the default.
func (r *ConnectionResolver) ResolveRedis(ctx context.Context, connectionID string) (RedisClient, error) {
	if connectionID == "" {
		return r.defaultRedis, nil
	}

	r.mu.Lock()
	if cached, ok := r.redisCache[connectionID]; ok {
		r.mu.Unlock()
		return cached.redis, nil
	}
	r.mu.Unlock()

	conn, err := r.store.GetByID(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("resolve redis connection %q: %w", connectionID, err)
	}
	if conn.Type != ConnectionTypeRedis {
		return nil, fmt.Errorf("connection %q is %s, expected redis", connectionID, conn.Type)
	}

	rdb := redis.NewClient(BuildRedisOpts(conn.Config))

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping redis %q: %w", connectionID, err)
	}

	rc := &redisClientImpl{rdb: rdb}

	r.mu.Lock()
	r.redisCache[connectionID] = resolvedRedis{client: rdb, redis: rc}
	r.mu.Unlock()

	return rc, nil
}

// ResolveConnection returns the raw Connection record by ID. Used by the
// skills system to extract a declared secret's value from an arbitrary
// Connection's config map. Bypasses the per-type caches because secrets
// don't need a live client — just the stored config.
func (r *ConnectionResolver) ResolveConnection(ctx context.Context, connectionID string) (Connection, error) {
	if connectionID == "" {
		return Connection{}, fmt.Errorf("resolve connection: connection_id is required")
	}
	conn, err := r.store.GetByID(ctx, connectionID)
	if err != nil {
		return Connection{}, fmt.Errorf("resolve connection %q: %w", connectionID, err)
	}
	return conn, nil
}

// Invalidate drops every cached client for the given connection ID so the
// next Resolve* rebuilds from the latest stored config. Safe to call from
// the connection upsert/delete handlers. DB/Redis cache entries are also
// closed so we do not leak connections after a config change.
func (r *ConnectionResolver) Invalidate(connectionID string) {
	if connectionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if v, ok := r.dbCache[connectionID]; ok {
		_ = v.client.Disconnect(context.Background())
		delete(r.dbCache, connectionID)
	}
	if v, ok := r.redisCache[connectionID]; ok {
		_ = v.client.Close()
		delete(r.redisCache, connectionID)
	}
	delete(r.llmCache, connectionID)
}

// Close disconnects all cached clients.
func (r *ConnectionResolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, v := range r.dbCache {
		_ = v.client.Disconnect(context.Background())
	}
	for _, v := range r.redisCache {
		_ = v.client.Close()
	}
	r.dbCache = make(map[string]resolvedDB)
	r.redisCache = make(map[string]resolvedRedis)
	return nil
}
