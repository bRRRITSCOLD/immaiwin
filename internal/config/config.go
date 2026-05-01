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
	Sandbox       SandboxConfig `envPrefix:"SANDBOX_"`
	Skills        SkillsConfig  `envPrefix:"SKILLS_"`
	Auth          AuthConfig    `envPrefix:"AUTH_"`
	Email         EmailConfig   `envPrefix:"EMAIL_"`
	EncryptionKey string        `env:"ENCRYPTION_KEY" envDefault:""`
}

// EmailConfig drives the transactional email sender (password reset
// + tenant invites today; future templates add here too). Provider
// "log" prints the message via slog so dev can copy reset/invite URLs
// from stdout. Provider "smtp" connects to the given host using
// standard net/smtp PLAIN auth + optional STARTTLS.
//
// SMTP host examples (real services that speak vanilla SMTP):
//   - SendGrid:    smtp.sendgrid.net:587   user="apikey", pass=<api_key>
//   - Mailgun:     smtp.mailgun.org:587
//   - AWS SES:     email-smtp.<region>.amazonaws.com:587
//   - Gmail (dev): smtp.gmail.com:587 (requires app password)
type EmailConfig struct {
	// Provider switches the sender impl: "log" | "smtp". Empty defaults
	// to "log" so dev boxes work out-of-the-box.
	Provider string `env:"PROVIDER" envDefault:"log"`
	// From is the envelope sender + From header. Required for SMTP.
	From string `env:"FROM" envDefault:""`
	// SMTP transport.
	SMTPHost     string `env:"SMTP_HOST"     envDefault:""`
	SMTPPort     int    `env:"SMTP_PORT"     envDefault:"587"`
	SMTPUser     string `env:"SMTP_USER"     envDefault:""`
	SMTPPassword string `env:"SMTP_PASSWORD" envDefault:""`
	// SMTPStartTLS upgrades a plaintext connection to TLS via the
	// STARTTLS extension. Standard for port 587. Set false only if
	// connecting to localhost mailcatcher / mailpit etc.
	SMTPStartTLS bool `env:"SMTP_STARTTLS" envDefault:"true"`
}

// AuthConfig drives JWT issuance + verification for the user-auth
// system. JWTSecret MUST be set in any non-dev deployment — the empty
// default is intentionally invalid so the API refuses to boot when the
// operator forgot to configure it.
type AuthConfig struct {
	JWTSecret         string `env:"JWT_SECRET"          envDefault:""`
	JWTTTL            string `env:"JWT_TTL"             envDefault:"168h"`
	CookieDomain      string `env:"COOKIE_DOMAIN"       envDefault:""`
	CookieSecure      bool   `env:"COOKIE_SECURE"       envDefault:"false"`
	AllowRegistration bool   `env:"ALLOW_REGISTRATION"  envDefault:"true"`
	// UIBaseURL is the absolute origin where the SPA lives. OAuth
	// callbacks redirect users back here after the provider hands us
	// a code. Defaults to the API host since dev typically proxies.
	UIBaseURL string `env:"UI_BASE_URL" envDefault:"http://localhost:3000"`
	// OAuthGoogle / OAuthGithub configure each provider. Empty
	// ClientID disables that provider's `/auth/oauth/:provider/start`
	// endpoint — `/auth/me` then surfaces only the providers that
	// actually have credentials wired.
	OAuthGoogle OAuthProviderConfig `envPrefix:"OAUTH_GOOGLE_"`
	OAuthGithub OAuthProviderConfig `envPrefix:"OAUTH_GITHUB_"`
}

// OAuthProviderConfig is one provider's client credentials + the
// redirect URL we register with them. Per-provider so a deployment
// can wire Google but not GitHub (or vice versa).
type OAuthProviderConfig struct {
	ClientID     string `env:"CLIENT_ID"     envDefault:""`
	ClientSecret string `env:"CLIENT_SECRET" envDefault:""`
	// RedirectURL is the absolute URL the provider redirects to with
	// the auth code. Must match what's registered in the provider's
	// console exactly. Default uses the API host on path
	// `/auth/oauth/<provider>/callback` — wire that path in the
	// provider console.
	RedirectURL string `env:"REDIRECT_URL"  envDefault:""`
}

// SkillsConfig configures the agent skills system (P1.9–P1.12).
// Disabled by default; set Enabled=true and provide a directory of bundles
// (or just rely on the Mongo registry) to expose skills to AI agents.
type SkillsConfig struct {
	Enabled bool `env:"ENABLED" envDefault:"false"`
	// Dir defaults to the project-local bundle directory so a freshly
	// `go run ./cmd/api`-from-repo-root setup picks up the in-tree
	// `skills/bundled/*` skills out of the gate. Prod deployments
	// override to an absolute path the API process owns (e.g.
	// `/var/lib/burrow/skills`).
	Dir string `env:"DIR" envDefault:"./skills/bundled"`
}

type APIConfig struct {
	Host        string `env:"HOST"     envDefault:"0.0.0.0"`
	Port        int    `env:"PORT"     envDefault:"8080"`
	BaseURL     string `env:"BASE_URL" envDefault:"https://localhost:8080"`
	TLSCertFile string `env:"TLS_CERT" envDefault:""`
	TLSKeyFile  string `env:"TLS_KEY"  envDefault:""`
}

type UIConfig struct {
	Host string `env:"HOST" envDefault:"0.0.0.0"`
	Port int    `env:"PORT" envDefault:"3000"`
}

type WorkerConfig struct {
	Concurrency int          `env:"CONCURRENCY" envDefault:"1"`
	Reaper      ReaperConfig `envPrefix:"REAPER_"`
}

// ReaperConfig drives the reaper worker — a periodic sweep that cancels
// runs stuck in non-terminal states (running / paused / pending_approval).
// TTLs are per-status because a 5-minute "running" timeout is right
// for normal flows but wrong for a human-in-the-loop approval that
// might legitimately wait an hour. Set any TTL to 0 to disable that
// status's sweep.
type ReaperConfig struct {
	// TickInterval is how often the reaper runs a sweep. Default 60s
	// — the sweep itself is cheap (one indexed query per status).
	TickInterval string `env:"TICK_INTERVAL" envDefault:"60s"`
	// RunningTTL caps wall-clock time for a single run. Catches dead
	// workers (crashed mid-run) and runaway agent loops. 1h matches
	// typical max workflow length; tune up for long-running batch.
	RunningTTL string `env:"RUNNING_TTL" envDefault:"1h"`
	// PausedTTL caps the breakpoint-paused state. Pre-exec breakpoints
	// hold the run mid-flow waiting for a UI "continue" frame; if
	// the operator walks away, the run sits forever.
	PausedTTL string `env:"PAUSED_TTL" envDefault:"4h"`
	// PendingApprovalTTL caps the OOB approval gate. Generous because
	// approvals legitimately wait on humans — Slack/email round-trips,
	// off-hours requests. 24h is the sweet spot before the run is
	// stale enough that auto-rejecting is preferable to leaving a
	// zombie.
	PendingApprovalTTL string `env:"PENDING_APPROVAL_TTL" envDefault:"24h"`
}

type RedisConfig struct {
	Host     string `env:"HOST" envDefault:"localhost"`
	Port     int    `env:"PORT" envDefault:"6379"`
	Password string `env:"PASSWORD" envDefault:""`
	DB       int    `env:"DB" envDefault:"0"`
}

type MongoDBConfig struct {
	URI      string `env:"URI" envDefault:"mongodb://localhost:27017"`
	Database string `env:"DATABASE" envDefault:"burrow"`
}

type SandboxConfig struct {
	Enabled       bool   `env:"ENABLED"        envDefault:"false"`
	Backend       string `env:"BACKEND"        envDefault:"docker"` // "docker" | "k3s" | "auto" (= docker)
	Runtime       string `env:"RUNTIME"        envDefault:""`       // Docker OCI runtime (e.g. "runsc" for gVisor)
	PoolSize      int    `env:"POOL_SIZE"      envDefault:"2"`      // Docker only — warm containers per language
	DockerHost    string `env:"DOCKER_HOST"    envDefault:""`       // override DOCKER_HOST env
	Kubeconfig    string `env:"KUBECONFIG"     envDefault:"/etc/rancher/k3s/k3s.yaml"`
	Namespace     string `env:"K3S_NAMESPACE"  envDefault:"burrow-sandbox"`
	RuntimeClass  string `env:"K3S_RUNTIMECLASS" envDefault:"gvisor"`
	ImageRegistry string `env:"IMAGE_REGISTRY" envDefault:""` // image registry prefix; required for k3s, optional for docker
	// Cluster CIDRs blocked by NetworkPolicy egress IPBlock Except (defense
	// against lateral movement). Defaults match k3s out-of-the-box. Stock
	// kubeadm typically uses 10.244.0.0/16 + 10.96.0.0/12; managed clusters
	// vary (EKS 192.168.0.0/16, GKE per-cluster, etc).
	PodCIDR       string `env:"POD_CIDR"      envDefault:"10.42.0.0/16"`
	ServiceCIDR   string `env:"SERVICE_CIDR"  envDefault:"10.43.0.0/16"`
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
