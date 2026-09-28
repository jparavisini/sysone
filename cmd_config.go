package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const configUsage = `sysone config - manage ~/.config/sysone/config.toml

Usage:
  sysone config init              write a starter config and the example specs
  sysone config show              print the effective config (keys masked)
  sysone config path              print the config file path
  sysone config edit              open the config in $EDITOR
  sysone config set KEY VALUE     set a dotted key, e.g. endpoints.default.url

Config file:
  default_endpoint = "default"

  [endpoints.default]
  url = "https://sysone.example.com"
  key_cmd = "pass show sysone/api-key"    # or key_env, key_file, key (plain)
  timeout = "30s"
  threshold = 0.9
  concurrency = 4
  # path = "/v1/systemone"                # request path under url
  # model = ""                            # sent as "model" in the body when set
  # headers = { "X-Team" = "ops" }
  # ca_file = "~/.config/sysone/ca.pem"
  # insecure = false

  [endpoints.openrouter]                  # Jev via OpenRouter
  url = "https://openrouter.ai/api"
  path = "/alpha/decisions"
  model = "typesafe/jev-1.13"
  key_env = "OPENROUTER_API_KEY"

  [endpoints.typesafe]                    # Jev direct
  url = "https://api.typesafe.ai"
  model = "jev-latest"
  key_env = "TYPESAFE_API_KEY"

Auth, first one set wins: --key, $SYSONE_KEY, key, key_env, key_file, key_cmd.
Location: $SYSONE_CONFIG, else $XDG_CONFIG_HOME/sysone/config.toml, else ~/.config/sysone/config.toml.

Examples:
  sysone config init
  sysone config set endpoints.default.url https://sysone.example.com
  sysone config set endpoints.default.key_cmd "op read op://Private/sysone/credential"
  sysone config set endpoints.openrouter.url https://openrouter.ai/api
  sysone config set endpoints.openrouter.path /alpha/decisions
  sysone config set endpoints.openrouter.model typesafe/jev-1.13
  sysone config set endpoints.openrouter.key_env OPENROUTER_API_KEY
`

const configTemplate = `# sysone configuration. See: sysone config --help
default_endpoint = "default"

[endpoints.default]
url = ""
# One of: key, key_env, key_file, key_cmd. $SYSONE_KEY and --key override all.
# key_cmd = "pass show sysone/api-key"
timeout = "30s"
threshold = 0.9
concurrency = 4
# path = "/v1/systemone"   # request path under url
# model = ""               # sent in the request body when set (hosted APIs need it)

# Jev via OpenRouter:
# [endpoints.openrouter]
# url = "https://openrouter.ai/api"
# path = "/alpha/decisions"
# model = "typesafe/jev-1.13"
# key_env = "OPENROUTER_API_KEY"
`

func cmdConfig(args []string) error {
	fs := newFlagSet("config", configUsage)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fmt.Fprint(stdout, configUsage)
		return &exitError{code: exitUsage}
	}
	switch pos[0] {
	case "init":
		return configInit()
	case "show":
		return configShow()
	case "path":
		fmt.Fprintln(stdout, configPath())
		return nil
	case "edit":
		return configEdit()
	case "set":
		if len(pos) != 3 {
			return fail(exitUsage, "usage: sysone config set KEY VALUE")
		}
		return configSet(pos[1], pos[2])
	}
	return fail(exitUsage, "unknown config subcommand %q\n\n%s", pos[0], configUsage)
}

func configInit() error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fail(exitBadInput, "%v", err)
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(p, []byte(configTemplate), 0o600); err != nil {
			return fail(exitBadInput, "%v", err)
		}
		fmt.Fprintf(stderr, "wrote %s\n", p)
	} else {
		fmt.Fprintf(stderr, "kept existing %s\n", p)
	}
	if err := os.MkdirAll(specsDir(), 0o755); err != nil {
		return fail(exitBadInput, "%v", err)
	}
	for _, name := range exampleSpecNames() {
		dst := filepath.Join(specsDir(), name+".json")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := os.WriteFile(dst, exampleSpec(name), 0o644); err != nil {
			return fail(exitBadInput, "%v", err)
		}
		fmt.Fprintf(stderr, "wrote %s\n", dst)
	}
	if cfg, err := loadConfig(); err == nil && cfg.Endpoints[cfg.DefaultEndpoint].URL == "" {
		fmt.Fprintf(stderr, "next: sysone config set endpoints.default.url https://your-server\n")
	}
	return nil
}

// configShow prints the config with every key value masked. key_cmd,
// key_env and key_file are locations, not secrets, and are shown.
func configShow() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for name, ep := range cfg.Endpoints {
		if ep.Key != "" {
			ep.Key = "****"
			cfg.Endpoints[name] = ep
		}
	}
	fmt.Fprintf(stdout, "# %s\n", configPath())
	return toml.NewEncoder(stdout).Encode(cfg)
}

func configEdit() error {
	editor := first(getenv("VISUAL"), getenv("EDITOR"), "vi")
	if _, err := os.Stat(configPath()); errors.Is(err, os.ErrNotExist) {
		if err := configInit(); err != nil {
			return err
		}
	}
	cmd := exec.Command("sh", "-c", editor+" "+strconv.Quote(configPath()))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fail(exitBadInput, "%s: %v", editor, err)
	}
	return nil
}

// configSet writes a dotted key. The file is decoded to a generic tree,
// updated and re-encoded, so comments are not preserved.
func configSet(key, value string) error {
	tree := map[string]any{}
	b, err := os.ReadFile(configPath())
	if err == nil {
		if _, err := toml.Decode(string(b), &tree); err != nil {
			return fail(exitBadInput, "parse %s: %v", configPath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(exitBadInput, "%v", err)
	}
	parts := strings.Split(key, ".")
	node := tree
	for _, p := range parts[:len(parts)-1] {
		child, ok := node[p].(map[string]any)
		if !ok {
			child = map[string]any{}
			node[p] = child
		}
		node = child
	}
	node[parts[len(parts)-1]] = tomlValue(value)

	var buf bytes.Buffer
	buf.WriteString("# sysone configuration. See: sysone config --help\n")
	if err := toml.NewEncoder(&buf).Encode(tree); err != nil {
		return fail(exitBadInput, "encode config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return fail(exitBadInput, "%v", err)
	}
	if err := os.WriteFile(configPath(), buf.Bytes(), 0o600); err != nil {
		return fail(exitBadInput, "%v", err)
	}
	if strings.HasSuffix(key, ".key") {
		fmt.Fprintf(stderr, "set %s (consider key_cmd or key_env instead of a plain key)\n", key)
	} else {
		fmt.Fprintf(stderr, "set %s = %s\n", key, value)
	}
	return nil
}

// tomlValue turns "true", "0.9", "4" into typed values; everything else is a string.
func tomlValue(v string) any {
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}
