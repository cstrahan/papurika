// Package config loads papurika's TOML configuration: named profiles (each a
// salt-api endpoint + credentials), XDG-compliant file locations, and secret
// sourcing via an exec helper instead of plaintext.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the parsed configuration file.
type Config struct {
	DefaultProfile string             `toml:"default_profile"`
	Profiles       map[string]Profile `toml:"profiles"`

	// SourcePath is the file this Config was read from, empty when no config
	// file was found (which is not an error — a fully flag/env-driven
	// invocation is valid). Set by Load; never read from the file itself.
	SourcePath string `toml:"-"`
}

// Profile is one named salt-api endpoint and its credentials.
type Profile struct {
	MasterURL string `toml:"master_url"`
	User      string `toml:"user"`
	Eauth     string `toml:"eauth"`

	// PasswordCommand is the preferred secret source: an argv whose stdout (first
	// line, trimmed) is the password. Keeps the secret out of this file.
	PasswordCommand []string `toml:"password_command"`
	// Password is a discouraged plaintext fallback.
	Password string `toml:"password"`

	VerifySSL *bool  `toml:"verify_ssl"` // nil => default true
	CACert    string `toml:"ca_cert"`
	Timeout   string `toml:"timeout"` // e.g. "120s"; client wall-clock budget
}

// ProfileSelection reports which profile an invocation will use, and why.
//
// The "why" matters because the fallback is easy to be surprised by: with no
// explicit choice, no PAPURIKA_PROFILE and no default_profile, papurika looks
// for a profile named DefaultProfileName, which may not exist.
type ProfileSelection struct {
	Name   string
	Source string // "explicit" | "env" | "default_profile" | "fallback"
	Exists bool   // whether a profile of that name is defined in the file
}

// SelectProfile reports which profile `explicit` (typically the -p flag)
// resolves to. Resolve uses this, so anything reporting the selection agrees
// with what actually gets used.
func (c *Config) SelectProfile(explicit string) ProfileSelection {
	sel := ProfileSelection{Name: explicit, Source: "explicit"}
	switch {
	case explicit != "":
	case os.Getenv("PAPURIKA_PROFILE") != "":
		sel = ProfileSelection{Name: os.Getenv("PAPURIKA_PROFILE"), Source: "env"}
	case c.DefaultProfile != "":
		sel = ProfileSelection{Name: c.DefaultProfile, Source: "default_profile"}
	default:
		sel = ProfileSelection{Name: DefaultProfileName, Source: "fallback"}
	}
	_, sel.Exists = c.Profiles[sel.Name]
	return sel
}

// Resolved is a fully-resolved profile after applying precedence
// (flags > env > file > defaults). It is what a Client is built from.
type Resolved struct {
	Profile   string
	MasterURL string
	User      string
	Eauth     string
	VerifySSL bool
	CACert    string
	Timeout   time.Duration
	Secret    SecretSource
}

// Overrides are higher-precedence values, typically from CLI flags. Empty fields
// are ignored.
type Overrides struct {
	MasterURL string
	User      string
	Eauth     string
	Insecure  bool // if true, forces VerifySSL=false
	CACert    string
	Timeout   string
	Password  string // explicit secret (e.g. --password / prompt); beats config
}

// DefaultProfileName is the profile used when nothing else selects one — no
// --profile, no PAPURIKA_PROFILE, no default_profile setting. Named to match
// the convention the AWS CLI established: a lone profile called "default" just
// works, with no config key pointing at it.
//
// Note it is the *last* resort. An explicit default_profile outranks it, so a
// config with both default_profile = "prod" and a [profiles.default] section
// uses prod — the explicit statement wins over the convention.
const DefaultProfileName = "default"

const (
	defaultEauth   = "pam"
	defaultTimeout = 120 * time.Second
)

// Load reads the config file. If paths are given, the first that exists is used;
// otherwise the XDG default is tried. A missing file is not an error — an empty
// Config is returned so a fully flag/env-driven invocation works.
func Load(paths ...string) (*Config, error) {
	if len(paths) == 0 {
		paths = DefaultConfigPaths()
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("config: reading %s: %w", p, err)
		}
		var c Config
		if err := toml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("config: parsing %s: %w", p, err)
		}
		c.SourcePath = p
		return &c, nil
	}
	return &Config{}, nil
}

// Resolve produces a fully-resolved profile applying precedence
// flags(ov) > env(PAPURIKA_*) > file > defaults.
func (c *Config) Resolve(profile string, ov Overrides) (*Resolved, error) {
	profile = c.SelectProfile(profile).Name
	p := c.Profiles[profile] // zero Profile if absent — that's fine if flags/env supply everything

	r := &Resolved{
		Profile:   profile,
		MasterURL: firstNonEmpty(ov.MasterURL, os.Getenv("PAPURIKA_URL"), p.MasterURL),
		User:      firstNonEmpty(ov.User, os.Getenv("PAPURIKA_USER"), p.User),
		Eauth:     firstNonEmpty(ov.Eauth, os.Getenv("PAPURIKA_EAUTH"), p.Eauth, defaultEauth),
		CACert:    firstNonEmpty(ov.CACert, os.Getenv("PAPURIKA_CACERT"), p.CACert),
	}
	if r.MasterURL == "" {
		return nil, fmt.Errorf("config: no master_url for profile %q (set it in config, --url, or PAPURIKA_URL)", profile)
	}

	// VerifySSL: default true; file may set it; --insecure or env force it off.
	r.VerifySSL = true
	if p.VerifySSL != nil {
		r.VerifySSL = *p.VerifySSL
	}
	if v := os.Getenv("PAPURIKA_VERIFY_SSL"); v != "" {
		r.VerifySSL = !isFalsey(v)
	}
	if ov.Insecure {
		r.VerifySSL = false
	}

	// Timeout.
	r.Timeout = defaultTimeout
	for _, s := range []string{ov.Timeout, os.Getenv("PAPURIKA_TIMEOUT"), p.Timeout} {
		if s == "" {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("config: invalid timeout %q: %w", s, err)
		}
		r.Timeout = d
		break
	}

	// Secret precedence: explicit override / env > password_command > plaintext.
	switch {
	case ov.Password != "":
		r.Secret = StaticSecret(ov.Password)
	case os.Getenv("PAPURIKA_PASSWORD") != "":
		r.Secret = StaticSecret(os.Getenv("PAPURIKA_PASSWORD"))
	case len(p.PasswordCommand) > 0:
		r.Secret = CommandSecret{Argv: p.PasswordCommand}
	case p.Password != "":
		r.Secret = StaticSecret(p.Password)
	default:
		r.Secret = nil // caller may prompt interactively
	}

	return r, nil
}

// DefaultConfigPaths returns the XDG config path followed by the legacy fallback.
func DefaultConfigPaths() []string {
	var paths []string
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		paths = append(paths, filepath.Join(x, "papurika", "config.toml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, ".config", "papurika", "config.toml"),
			filepath.Join(home, ".papurika.toml"), // legacy fallback
		)
	}
	return paths
}

// CacheDir returns the XDG cache directory for papurika (token cache lives here).
func CacheDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, "papurika")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "papurika")
	}
	return filepath.Join(os.TempDir(), "papurika")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isFalsey(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}
