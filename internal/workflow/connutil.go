package workflow

import (
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bRRRITSCOLD/immaiwin-go/internal/config"
	"github.com/bRRRITSCOLD/immaiwin-go/internal/schwab"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// BuildMongoOpts creates a mongo ClientOptions from a config map.
// URI is always applied first; other keys layer on top.
func BuildMongoOpts(cfg map[string]string) *options.ClientOptions {
	opts := options.Client().ApplyURI(cfg["uri"])

	// Auth
	if cfg["username"] != "" || cfg["password"] != "" {
		cred := options.Credential{
			Username: cfg["username"],
			Password: cfg["password"],
		}
		if cfg["auth_source"] != "" {
			cred.AuthSource = cfg["auth_source"]
		}
		if cfg["auth_mechanism"] != "" {
			cred.AuthMechanism = cfg["auth_mechanism"]
		}
		if cfg["password"] != "" {
			cred.PasswordSet = true
		}
		opts = opts.SetAuth(cred)
	}

	// Pool
	if v, err := strconv.ParseUint(cfg["max_pool_size"], 10, 64); err == nil {
		opts = opts.SetMaxPoolSize(v)
	}
	if v, err := strconv.ParseUint(cfg["min_pool_size"], 10, 64); err == nil {
		opts = opts.SetMinPoolSize(v)
	}
	if v, err := strconv.ParseUint(cfg["max_connecting"], 10, 64); err == nil {
		opts = opts.SetMaxConnecting(v)
	}
	if d, err := time.ParseDuration(cfg["max_conn_idle_time"]); err == nil {
		opts = opts.SetMaxConnIdleTime(d)
	}

	// Timeouts
	if d, err := time.ParseDuration(cfg["connect_timeout"]); err == nil {
		opts = opts.SetConnectTimeout(d)
	}
	if d, err := time.ParseDuration(cfg["server_selection_timeout"]); err == nil {
		opts = opts.SetServerSelectionTimeout(d)
	}
	if d, err := time.ParseDuration(cfg["heartbeat_interval"]); err == nil {
		opts = opts.SetHeartbeatInterval(d)
	}
	if d, err := time.ParseDuration(cfg["timeout"]); err == nil {
		opts = opts.SetTimeout(d)
	}

	// Replica set & read/write
	if cfg["replica_set"] != "" {
		opts = opts.SetReplicaSet(cfg["replica_set"])
	}
	if rp := parseReadPref(cfg["read_preference"]); rp != nil {
		opts = opts.SetReadPreference(rp)
	}
	if cfg["write_concern_w"] != "" {
		wc := parseWriteConcern(cfg["write_concern_w"])
		opts = opts.SetWriteConcern(wc)
	}
	if cfg["direct"] == "true" {
		opts = opts.SetDirect(true)
	}
	if cfg["retry_reads"] == "true" {
		opts = opts.SetRetryReads(true)
	}
	if cfg["retry_writes"] == "true" {
		opts = opts.SetRetryWrites(true)
	}

	// TLS
	if cfg["tls"] == "true" {
		tc := &tls.Config{}
		if cfg["tls_insecure"] == "true" {
			tc.InsecureSkipVerify = true //nolint:gosec // user-opted
		}
		opts = opts.SetTLSConfig(tc)
	}

	// Compression
	if cfg["compressors"] != "" {
		opts = opts.SetCompressors(strings.Split(cfg["compressors"], ","))
	}
	if v, err := strconv.Atoi(cfg["zlib_level"]); err == nil {
		opts = opts.SetZlibLevel(v)
	}
	if v, err := strconv.Atoi(cfg["zstd_level"]); err == nil {
		opts = opts.SetZstdLevel(v)
	}

	// Advanced
	if cfg["app_name"] != "" {
		opts = opts.SetAppName(cfg["app_name"])
	}
	if d, err := time.ParseDuration(cfg["local_threshold"]); err == nil {
		opts = opts.SetLocalThreshold(d)
	}
	if v, err := strconv.Atoi(cfg["srv_max_hosts"]); err == nil {
		opts = opts.SetSRVMaxHosts(v)
	}
	if cfg["load_balanced"] == "true" {
		opts = opts.SetLoadBalanced(true)
	}

	return opts
}

// BuildRedisOpts creates a redis.Options from a config map.
func BuildRedisOpts(cfg map[string]string) *redis.Options {
	port := 6379
	if p, err := strconv.Atoi(cfg["port"]); err == nil && p > 0 {
		port = p
	}
	db := 0
	if d, err := strconv.Atoi(cfg["db"]); err == nil {
		db = d
	}

	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", cfg["host"], port),
		Password: cfg["password"],
		DB:       db,
	}

	// Auth
	if cfg["username"] != "" {
		opts.Username = cfg["username"]
	}

	// Client name
	if cfg["client_name"] != "" {
		opts.ClientName = cfg["client_name"]
	}

	// Network
	if cfg["network"] == "unix" {
		opts.Network = "unix"
		opts.Addr = cfg["host"] // unix socket path
	}

	// Protocol
	if v, err := strconv.Atoi(cfg["protocol"]); err == nil {
		opts.Protocol = v
	}

	// Pool
	if v, err := strconv.Atoi(cfg["pool_size"]); err == nil {
		opts.PoolSize = v
	}
	if v, err := strconv.Atoi(cfg["min_idle_conns"]); err == nil {
		opts.MinIdleConns = v
	}
	if v, err := strconv.Atoi(cfg["max_idle_conns"]); err == nil {
		opts.MaxIdleConns = v
	}
	if v, err := strconv.Atoi(cfg["max_active_conns"]); err == nil {
		opts.MaxActiveConns = v
	}
	if d, err := time.ParseDuration(cfg["pool_timeout"]); err == nil {
		opts.PoolTimeout = d
	}
	if d, err := time.ParseDuration(cfg["conn_max_idle_time"]); err == nil {
		opts.ConnMaxIdleTime = d
	}
	if d, err := time.ParseDuration(cfg["conn_max_lifetime"]); err == nil {
		opts.ConnMaxLifetime = d
	}
	if cfg["pool_fifo"] == "true" {
		opts.PoolFIFO = true
	}

	// Timeouts
	if d, err := time.ParseDuration(cfg["dial_timeout"]); err == nil {
		opts.DialTimeout = d
	}
	if d, err := time.ParseDuration(cfg["read_timeout"]); err == nil {
		opts.ReadTimeout = d
	}
	if d, err := time.ParseDuration(cfg["write_timeout"]); err == nil {
		opts.WriteTimeout = d
	}

	// Retries
	if v, err := strconv.Atoi(cfg["max_retries"]); err == nil {
		opts.MaxRetries = v
	}
	if d, err := time.ParseDuration(cfg["min_retry_backoff"]); err == nil {
		opts.MinRetryBackoff = d
	}
	if d, err := time.ParseDuration(cfg["max_retry_backoff"]); err == nil {
		opts.MaxRetryBackoff = d
	}

	// TLS
	if cfg["tls"] == "true" {
		tc := &tls.Config{}
		if cfg["tls_insecure"] == "true" {
			tc.InsecureSkipVerify = true //nolint:gosec // user-opted
		}
		opts.TLSConfig = tc
	}

	// Advanced
	if cfg["context_timeout_enabled"] == "true" {
		opts.ContextTimeoutEnabled = true
	}

	return opts
}

// BuildRabbitMQURL creates an AMQP URI from a config map.
// Keys: host (required), port (default 5672), username, password, vhost (default /).
func BuildRabbitMQURL(cfg map[string]string) string {
	host := cfg["host"]
	port := "5672"
	if p := cfg["port"]; p != "" {
		port = p
	}
	vhost := "/"
	if v := cfg["vhost"]; v != "" {
		vhost = v
	}

	userInfo := ""
	if cfg["username"] != "" || cfg["password"] != "" {
		userInfo = cfg["username"] + ":" + cfg["password"] + "@"
	}

	return fmt.Sprintf("amqp://%s%s:%s/%s", userInfo, host, port, strings.TrimPrefix(vhost, "/"))
}

// BuildSchwabTokenManager creates a per-connection schwab.TokenManager from a connection config map.
func BuildSchwabTokenManager(cfg map[string]string, db *mongo.Database, connID string) *schwab.TokenManager {
	return schwab.NewTokenManagerWithID(config.SchwabConfig{
		ClientID:     cfg["client_id"],
		ClientSecret: cfg["client_secret"],
		CallbackURL:  cfg["callback_url"],
	}, db, "conn_tokens:"+connID)
}

func parseReadPref(s string) *readpref.ReadPref {
	switch s {
	case "primaryPreferred":
		return readpref.PrimaryPreferred()
	case "secondary":
		return readpref.Secondary()
	case "secondaryPreferred":
		return readpref.SecondaryPreferred()
	case "nearest":
		return readpref.Nearest()
	default:
		return nil
	}
}

func parseWriteConcern(w string) *writeconcern.WriteConcern {
	if w == "majority" {
		return writeconcern.Majority()
	}
	if v, err := strconv.Atoi(w); err == nil {
		return &writeconcern.WriteConcern{W: v}
	}
	return nil
}
