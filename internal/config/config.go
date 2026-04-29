package config

import (
	"github.com/bRRRITSCOLD/enviro-go"
)

type Config struct {
	API           APIConfig     `envPrefix:"API_"`
	UI            UIConfig      `envPrefix:"UI_"`
	Worker        WorkerConfig  `envPrefix:"WORKER_"`
	Redis         RedisConfig   `envPrefix:"REDIS_"`
	MongoDB       MongoDBConfig `envPrefix:"MONGODB_"`
	Schwab        SchwabConfig  `envPrefix:"SCHWAB_"`
	Sandbox       SandboxConfig `envPrefix:"SANDBOX_"`
	Skills        SkillsConfig  `envPrefix:"SKILLS_"`
	Auth          AuthConfig    `envPrefix:"AUTH_"`
	EncryptionKey string        `env:"ENCRYPTION_KEY" envDefault:""`
}

// AuthConfig drives JWT issuance + verification for the user-auth
// system. JWTSecret MUST be set in any non-dev deployment — the empty
// default is intentionally invalid so the API refuses to boot when the
// operator forgot to configure it.
type AuthConfig struct {
	// JWTSecret is the HS256 signing key. Recommend 32+ random bytes
	// hex-encoded. Used for both UI cookie tokens and API Bearer
	// tokens (when issued via /auth/login). API keys live in a
	// separate collection and don't go through JWT signing.
	JWTSecret string `env:"JWT_SECRET" envDefault:""`
	// JWTTTL controls how long a freshly-issued JWT remains valid
	// (e.g. "24h", "168h" for a week). Refresh = re-login until we
	// add a refresh-token flow.
	JWTTTL string `env:"JWT_TTL" envDefault:"168h"`
	// CookieDomain scopes the auth cookie when set. Empty = host-only.
	CookieDomain string `env:"COOKIE_DOMAIN" envDefault:""`
	// CookieSecure enforces the Secure flag on the auth cookie.
	// Disabled in dev (HTTP). Set true behind HTTPS.
	CookieSecure bool `env:"COOKIE_SECURE" envDefault:"false"`
	// AllowRegistration toggles the public /auth/register endpoint.
	// In a closed deployment, set false + create users via CLI / admin
	// UI (admin UI deferred backlog).
	AllowRegistration bool `env:"ALLOW_REGISTRATION" envDefault:"true"`
}

// SkillsConfig configures the agent skills system (P1.9–P1.12).
// Disabled by default; set Enabled=true and provide a directory of bundles
// (or just rely on the Mongo registry) to expose skills to AI agents.
type SkillsConfig struct {
	Enabled bool   `env:"ENABLED" envDefault:"false"`
	Dir     string `env:"DIR"     envDefault:"/var/lib/immaiwin/skills"`
}

type APIConfig struct {
	Host        string `env:"HOST"     envDefault:"0.0.0.0"`
	Port        int    `env:"PORT"     envDefault:"8080"`
	BaseURL     string `env:"BASE_URL" envDefault:"https://127.0.0.1:8080"`
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

type SandboxConfig struct {
	Enabled       bool   `env:"ENABLED"        envDefault:"false"`
	Backend       string `env:"BACKEND"        envDefault:"docker"`           // "docker" | "k3s" | "auto" (= docker)
	Runtime       string `env:"RUNTIME"        envDefault:""`                 // Docker OCI runtime (e.g. "runsc" for gVisor)
	PoolSize      int    `env:"POOL_SIZE"      envDefault:"2"`                // Docker only — warm containers per language
	DockerHost    string `env:"DOCKER_HOST"    envDefault:""`                 // override DOCKER_HOST env
	Kubeconfig    string `env:"KUBECONFIG"     envDefault:"/etc/rancher/k3s/k3s.yaml"`
	Namespace     string `env:"K3S_NAMESPACE"  envDefault:"immaiwin-sandbox"`
	RuntimeClass  string `env:"K3S_RUNTIMECLASS" envDefault:"gvisor"`
	ImageRegistry string `env:"IMAGE_REGISTRY" envDefault:""`                 // image registry prefix; required for k3s, optional for docker
	// Cluster CIDRs blocked by NetworkPolicy egress IPBlock Except (defense
	// against lateral movement). Defaults match k3s out-of-the-box. Stock
	// kubeadm typically uses 10.244.0.0/16 + 10.96.0.0/12; managed clusters
	// vary (EKS 192.168.0.0/16, GKE per-cluster, etc).
	PodCIDR      string `env:"POD_CIDR"      envDefault:"10.42.0.0/16"`
	ServiceCIDR  string `env:"SERVICE_CIDR"  envDefault:"10.43.0.0/16"`
	LinkLocalCIDR string `env:"LINKLOCAL_CIDR" envDefault:"169.254.0.0/16"`
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
