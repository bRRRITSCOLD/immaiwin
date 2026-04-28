package workflow

import (
	"context"
	"fmt"
	"sync"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/llm"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ConnectionResolver resolves connection IDs to live DB/Pub/LLM clients.
// Empty connection ID returns the default client (or an error for LLM,
// which has no default). Resolved clients are cached per ID.
type ConnectionResolver struct {
	store      ConnectionStore
	defaultDB  RawUpserter
	defaultPub Publisher
	mu         sync.Mutex
	dbCache    map[string]resolvedDB
	pubCache   map[string]resolvedPub
	llmCache   map[string]llm.Provider
}

type resolvedDB struct {
	client   *mongo.Client
	upserter RawUpserter
}

type resolvedPub struct {
	client *redis.Client
	pub    Publisher
}

// redisPublisher wraps *redis.Client to satisfy Publisher.
type redisPublisher struct {
	rdb *redis.Client
}

func (p *redisPublisher) Publish(ctx context.Context, channel string, payload []byte) error {
	return p.rdb.Publish(ctx, channel, payload).Err()
}

// rawDB wraps *mongo.Database to satisfy RawUpserter — mirrors mongodb.RawDB but avoids circular import.
type rawDB struct {
	db *mongo.Database
}

func (r *rawDB) UpsertRaw(ctx context.Context, collection string, filter, update bson.M, upsert bool) (int64, int64, error) {
	res, err := r.db.Collection(collection).UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(upsert))
	if err != nil {
		return 0, 0, err
	}
	return res.MatchedCount, res.UpsertedCount, nil
}

func NewConnectionResolver(store ConnectionStore, defaultDB RawUpserter, defaultPub Publisher) *ConnectionResolver {
	return &ConnectionResolver{
		store:      store,
		defaultDB:  defaultDB,
		defaultPub: defaultPub,
		dbCache:    make(map[string]resolvedDB),
		pubCache:   make(map[string]resolvedPub),
		llmCache:   make(map[string]llm.Provider),
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

// ResolveDB returns a RawUpserter for the given connection ID.
// Empty ID returns the default.
func (r *ConnectionResolver) ResolveDB(ctx context.Context, connectionID string) (RawUpserter, error) {
	if connectionID == "" {
		return r.defaultDB, nil
	}

	r.mu.Lock()
	if cached, ok := r.dbCache[connectionID]; ok {
		r.mu.Unlock()
		return cached.upserter, nil
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
		database = "immaiwin"
	}
	upserter := &rawDB{db: client.Database(database)}

	r.mu.Lock()
	r.dbCache[connectionID] = resolvedDB{client: client, upserter: upserter}
	r.mu.Unlock()

	return upserter, nil
}

// ResolvePub returns a Publisher for the given connection ID.
// Empty ID returns the default.
func (r *ConnectionResolver) ResolvePub(ctx context.Context, connectionID string) (Publisher, error) {
	if connectionID == "" {
		return r.defaultPub, nil
	}

	r.mu.Lock()
	if cached, ok := r.pubCache[connectionID]; ok {
		r.mu.Unlock()
		return cached.pub, nil
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

	pub := &redisPublisher{rdb: rdb}

	r.mu.Lock()
	r.pubCache[connectionID] = resolvedPub{client: rdb, pub: pub}
	r.mu.Unlock()

	return pub, nil
}

// Close disconnects all cached clients.
func (r *ConnectionResolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, v := range r.dbCache {
		_ = v.client.Disconnect(context.Background())
	}
	for _, v := range r.pubCache {
		_ = v.client.Close()
	}
	r.dbCache = make(map[string]resolvedDB)
	r.pubCache = make(map[string]resolvedPub)
	return nil
}
