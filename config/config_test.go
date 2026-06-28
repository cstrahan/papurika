package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolvePrecedence(t *testing.T) {
	path := writeTOML(t, `
default_profile = "prod"

[profiles.prod]
master_url = "https://master:8000"
user = "alice"
eauth = "file"
password = "filesecret"
timeout = "30s"

[profiles.gcp]
master_url = "http://gcp:8000"
user = "bob"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Default profile from file.
	r, err := cfg.Resolve("", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != "prod" || r.MasterURL != "https://master:8000" || r.User != "alice" || r.Eauth != "file" {
		t.Errorf("default resolve wrong: %+v", r)
	}
	if r.Timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", r.Timeout)
	}
	if !r.VerifySSL {
		t.Errorf("VerifySSL should default true")
	}

	// Flag override beats file.
	r, err = cfg.Resolve("prod", Overrides{User: "carol", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.User != "carol" {
		t.Errorf("override user = %q, want carol", r.User)
	}
	if r.VerifySSL {
		t.Errorf("--insecure should force VerifySSL=false")
	}

	// Env beats file, flag beats env.
	t.Setenv("PAPURIKA_USER", "dave")
	r, _ = cfg.Resolve("prod", Overrides{})
	if r.User != "dave" {
		t.Errorf("env user = %q, want dave", r.User)
	}
	r, _ = cfg.Resolve("prod", Overrides{User: "erin"})
	if r.User != "erin" {
		t.Errorf("flag should beat env: %q", r.User)
	}

	// Unconfigured profile with no url is an error.
	if _, err := cfg.Resolve("missing", Overrides{}); err == nil {
		t.Errorf("expected error for profile with no master_url")
	}
}

func TestSecretPrecedence(t *testing.T) {
	path := writeTOML(t, `
[profiles.p]
master_url = "https://m:8000"
password = "plain"
password_command = ["printf", "fromcmd"]
`)
	cfg, _ := Load(path)

	// password_command beats plaintext password.
	r, err := cfg.Resolve("p", Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := r.Secret.Secret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sec != "fromcmd" {
		t.Errorf("secret = %q, want fromcmd (command beats plaintext)", sec)
	}

	// Explicit override beats everything.
	r, _ = cfg.Resolve("p", Overrides{Password: "override"})
	sec, _ = r.Secret.Secret(context.Background())
	if sec != "override" {
		t.Errorf("secret = %q, want override", sec)
	}
}

func TestCommandSecretTrimsToFirstLine(t *testing.T) {
	s := CommandSecret{Argv: []string{"printf", "line1\nline2\n"}}
	got, err := s.Secret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "line1" {
		t.Errorf("got %q, want line1", got)
	}
}

func TestLoadMissingFileOK(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(cfg.Profiles) != 0 {
		t.Errorf("expected empty config")
	}
}
