package config

import (
	"github.com/bRRRITSCOLD/enviro-go"
)

type Config struct {
	API     APIConfig     `envPrefix:"API_"`
	UI      UIConfig      `envPrefix:"UI_"`
	Worker  WorkerConfig  `envPrefix:"WORKER_"`
	Redis   RedisConfig   `envPrefix:"REDIS_"`
	MongoDB MongoDBConfig `envPrefix:"MONGODB_"`
	Schwab  SchwabConfig  `envPrefix:"SCHWAB_"`
}

type APIConfig struct {
	Host        string `env:"HOST"     envDefault:"0.0.0.0"`
	Port        int    `env:"PORT"     envDefault:"8080"`
	TLSCertFile string `env:"TLS_CERT" envDefault:""`
	TLSKeyFile  string `env:"TLS_KEY"  envDefault:""`
}

type UIConfig struct {
	Host string `env:"HOST" envDefault:"0.0.0.0"`
	Port int    `env:"PORT" envDefault:"3000"`
}

type WorkerConfig struct {
	Concurrency int `env:"CONCURRENCY" envDefault:"1"`
}

type RedisConfig struct {
	Host     string `env:"HOST" envDefault:"localhost"`
	Port     int    `env:"PORT" envDefault:"6379"`
	Password string `env:"PASSWORD" envDefault:""`
	DB       int    `env:"DB" envDefault:"0"`
}

type MongoDBConfig struct {
	URI      string `env:"URI" envDefault:"mongodb://localhost:27017"`
	Database string `env:"DATABASE" envDefault:"immaiwin"`
}

type SchwabConfig struct {
	ClientID     string `env:"CLIENT_ID"     envDefault:""`
	ClientSecret string `env:"CLIENT_SECRET" envDefault:""`
	CallbackURL  string `env:"CALLBACK_URL"  envDefault:"https://127.0.0.1:8080/auth/schwab/callback"`
}

func Load(opts ...Option) (*Config, error) {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}

	e, err := enviro.Parse[Config](enviro.EnvConfig{Path: o.dotEnvPath})
	if err != nil {
		return nil, err
	}

	cfg := e.Config()
	return &cfg, nil
}

type options struct {
	dotEnvPath string
}

// Option configures Load behaviour.
type Option func(*options)

// WithDotEnv loads environment variables from the given file path before parsing.
func WithDotEnv(path string) Option {
	return func(o *options) { o.dotEnvPath = path }
}
