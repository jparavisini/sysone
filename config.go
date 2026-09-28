package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultTimeout   = 30 * time.Second
	defaultThreshold = 0.9
	defaultPath      = "/v1/systemone"
)

// Config is the on-disk shape of ~/.config/sysone/config.toml.
type Config struct {
	DefaultEndpoint string              `toml:"default_endpoint"`
	Endpoints       map[string]Endpoint `toml:"endpoints"`
}

// Endpoint is one named server in the config file.
type Endpoint struct {
	URL         string            `toml:"url"`
	Path        string            `toml:"path,omitempty"`
	Model       string            `toml:"model,omitempty"`
	Key         string            `toml:"key,omitempty"`
	KeyEnv      string            `toml:"key_env,omitempty"`
	KeyFile     string            `toml:"key_file,omitempty"`
	KeyCmd      string            `toml:"key_cmd,omitempty"`
	Timeout     string            `toml:"timeout,omitempty"`
	Headers     map[string]string `toml:"headers,omitempty"`
	CAFile      string            `toml:"ca_file,omitempty"`
	Insecure    bool              `toml:"insecure,omitempty"`
	Concurrency int               `toml:"concurrency,omitempty"`
	Threshold   *float64          `toml:"threshold,omitempty"`
}

// configDir respects SYSONE_CONFIG (a file path) and XDG_CONFIG_HOME.
func configPath() string {
	if p := getenv("SYSONE_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(configDir(), "config.toml")
}

func configDir() string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "sysone")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sysone")
}

func specsDir() string { return filepath.Join(configDir(), "specs") }

// loadConfig reads the config file. A missing file yields the built-in
// default: one endpoint named "default" with no URL, key from $SYSONE_KEY.
func loadConfig() (*Config, error) {
	cfg := &Config{DefaultEndpoint: "default", Endpoints: map[string]Endpoint{}}
	b, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		cfg.Endpoints["default"] = Endpoint{}
		return cfg, nil
	}
	if err != nil {
		return nil, fail(exitBadInput, "read config: %v", err)
	}
	if _, err := toml.Decode(string(b), cfg); err != nil {
		return nil, fail(exitBadInput, "parse %s: %v", configPath(), err)
	}
	if cfg.DefaultEndpoint == "" {
		cfg.DefaultEndpoint = "default"
	}
	if cfg.Endpoints == nil {
		cfg.Endpoints = map[string]Endpoint{}
	}
	return cfg, nil
}

// Resolved is the fully merged endpoint settings for one invocation:
// flags > env > config > defaults.
type Resolved struct {
	Name        string
	URL         string
	Path        string
	Model       string
	Timeout     time.Duration
	Headers     map[string]string
	CAFile      string
	Insecure    bool
	Concurrency int
	Threshold   float64
	Verbose     bool
	Raw         bool

	keySource func() (string, error)
	keyOnce   sync.Once
	key       string
	keyErr    error
}

// Key resolves the API key once per process and caches it in memory.
// Safe for concurrent use by batch workers.
func (r *Resolved) Key() (string, error) {
	r.keyOnce.Do(func() {
		k, err := r.keySource()
		r.key, r.keyErr = strings.TrimSpace(k), err
	})
	return r.key, r.keyErr
}

// resolve merges the global flags, environment and config for one endpoint.
func resolve(g *globalFlags) (*Resolved, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return resolveWith(cfg, g)
}

func resolveWith(cfg *Config, g *globalFlags) (*Resolved, error) {
	name := first(g.endpoint, getenv("SYSONE_ENDPOINT"), cfg.DefaultEndpoint)
	ep, ok := cfg.Endpoints[name]
	if !ok {
		if g.url == "" && getenv("SYSONE_URL") == "" {
			return nil, fail(exitUsage, "unknown endpoint %q in %s", name, configPath())
		}
		ep = Endpoint{}
	}

	r := &Resolved{
		Name:        name,
		URL:         strings.TrimRight(first(g.url, getenv("SYSONE_URL"), ep.URL), "/"),
		Path:        first(ep.Path, defaultPath),
		Model:       first(g.model, getenv("SYSONE_MODEL"), ep.Model),
		Headers:     ep.Headers,
		CAFile:      ep.CAFile,
		Insecure:    ep.Insecure,
		Concurrency: ep.Concurrency,
		Threshold:   defaultThreshold,
		Verbose:     g.verbose,
		Raw:         g.raw,
	}
	if r.URL == "" {
		return nil, fail(exitUsage, "endpoint %q has no url: set SYSONE_URL, pass --url, or run `sysone config init`", name)
	}
	if r.Concurrency < 1 {
		r.Concurrency = 1
	}

	r.Timeout = defaultTimeout
	if t := first(g.timeout, getenv("SYSONE_TIMEOUT"), ep.Timeout); t != "" {
		d, err := parseTimeout(t)
		if err != nil {
			return nil, err
		}
		r.Timeout = d
	}

	if ep.Threshold != nil {
		r.Threshold = *ep.Threshold
	}
	if t := getenv("SYSONE_THRESHOLD"); t != "" {
		v, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil, fail(exitUsage, "SYSONE_THRESHOLD: %v", err)
		}
		r.Threshold = v
	}
	if g.threshold >= 0 {
		r.Threshold = g.threshold
	}
	if r.Threshold < 0 || r.Threshold > 1 {
		return nil, fail(exitUsage, "threshold must be between 0 and 1, got %v", r.Threshold)
	}

	r.keySource = keySource(g, ep)
	return r, nil
}

// keySource picks the first configured auth source. Order: --key,
// $SYSONE_KEY, then the endpoint's key, key_env, key_file, key_cmd.
func keySource(g *globalFlags, ep Endpoint) func() (string, error) {
	switch {
	case g.key != "":
		return literal(g.key)
	case getenv("SYSONE_KEY") != "":
		return literal(getenv("SYSONE_KEY"))
	case ep.Key != "":
		return literal(ep.Key)
	case ep.KeyEnv != "":
		return func() (string, error) {
			v := getenv(ep.KeyEnv)
			if v == "" {
				return "", fail(exitAuth, "key_env %s is not set", ep.KeyEnv)
			}
			return v, nil
		}
	case ep.KeyFile != "":
		return func() (string, error) {
			p := expandHome(ep.KeyFile)
			fi, err := os.Stat(p)
			if err != nil {
				return "", fail(exitAuth, "key_file: %v", err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(stderr, "sysone: warning: %s is mode %04o; run chmod 600\n", p, fi.Mode().Perm())
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return "", fail(exitAuth, "key_file: %v", err)
			}
			return string(b), nil
		}
	case ep.KeyCmd != "":
		return func() (string, error) {
			cmd := exec.Command("sh", "-c", ep.KeyCmd)
			cmd.Stderr = stderr
			out, err := cmd.Output()
			if err != nil {
				return "", fail(exitAuth, "key_cmd failed: %v", err)
			}
			// Only the first line: password managers often append metadata.
			k, _, _ := strings.Cut(string(out), "\n")
			if strings.TrimSpace(k) == "" {
				return "", fail(exitAuth, "key_cmd printed nothing")
			}
			return k, nil
		}
	}
	return func() (string, error) { return "", nil }
}

func literal(s string) func() (string, error) {
	return func() (string, error) { return s, nil }
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseTimeout accepts Go durations ("30s", "1m") or bare seconds ("30").
func parseTimeout(s string) (time.Duration, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fail(exitUsage, "bad timeout %q: use e.g. 30s or 2m", s)
	}
	return d, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
