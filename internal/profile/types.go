package profile

// SchedulePitcherProfile defines which checks to run, when, and where to pitch
// the results. It is usually mounted from a ConfigMap.
type SchedulePitcherProfile struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type Spec struct {
	Pitcher  PitcherConfig `yaml:"pitcher"`
	Redis    RedisConfig   `yaml:"redis"`
	Defaults Defaults      `yaml:"defaults"`
	Checks   []Check       `yaml:"checks"`
}

// Pitcher formats.
const (
	FormatGrafana = "grafana"
	FormatGeneric = "generic"
)

type PitcherConfig struct {
	// Addr is the full pitch URL, e.g. https://omni.example/pitch/grafana.
	Addr string `yaml:"addr"`
	// Format is grafana (POST /pitch/grafana) or generic (POST /pitch).
	Format   string     `yaml:"format"`
	CAFile   string     `yaml:"caFile"`
	Insecure bool       `yaml:"insecure"`
	Auth     AuthConfig `yaml:"auth"`
}

type AuthConfig struct {
	Token     string     `yaml:"token"`
	TokenFrom *ValueFrom `yaml:"tokenFrom"`
}

type RedisConfig struct {
	Addr         string     `yaml:"addr"`
	Port         string     `yaml:"port"`
	Password     string     `yaml:"password"`
	PasswordFrom *ValueFrom `yaml:"passwordFrom"`
	// Prefix of all keys, default homerun2-schedule-pitcher.
	Prefix string `yaml:"prefix"`
}

// ValueFrom points to a secret value. Exactly one source must be set.
type ValueFrom struct {
	SecretKeyRef *SecretKeyRef `yaml:"secretKeyRef"`
	Env          string        `yaml:"env"`
	File         string        `yaml:"file"`
}

type SecretKeyRef struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Key       string `yaml:"key"`
}

type Defaults struct {
	Schedule   string     `yaml:"schedule"`
	Timezone   string     `yaml:"timezone"`
	Remind     string     `yaml:"remind"`
	Thresholds Thresholds `yaml:"thresholds"`
	System     string     `yaml:"system"`
	Tags       []string   `yaml:"tags"`
	// Assignee is used for checks without their own assignee.
	Assignee string `yaml:"assignee"`
}

// Thresholds are the remaining times at or below which a check enters a band.
type Thresholds struct {
	Warning  Duration `yaml:"warning"`
	Error    Duration `yaml:"error"`
	Critical Duration `yaml:"critical"`
}

// Check types.
const (
	TypeGitHubTokenExpiry = "github-token-expiry"
	TypeTLSEndpoint       = "tls-endpoint"
)

// Check is one scheduled check. Empty fields fall back to spec.defaults.
type Check struct {
	ID          string `yaml:"id"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Schedule    string `yaml:"schedule"`
	Remind      string `yaml:"remind"`
	// Thresholds override the defaults field by field.
	Thresholds Thresholds `yaml:"thresholds"`
	Tags       []string   `yaml:"tags"`
	URL        string     `yaml:"url"`
	Assignee   string     `yaml:"assignee"`
	Paused     bool       `yaml:"paused"`

	// github-token-expiry
	TokenFrom *ValueFrom `yaml:"tokenFrom"`
	// APIURL defaults to https://api.github.com (set it for GitHub Enterprise).
	APIURL string `yaml:"apiURL"`
	// Owner looks up the token owner with GET /user (default true).
	Owner *bool `yaml:"owner"`
	// Reminder keeps a reminder in sync with the learned expiry (not implemented yet).
	Reminder bool `yaml:"reminder"`

	// tls-endpoint
	Target     string   `yaml:"target"`
	ServerName string   `yaml:"serverName"`
	CAFile     string   `yaml:"caFile"`
	Chain      bool     `yaml:"chain"`
	Timeout    Duration `yaml:"timeout"`
}
