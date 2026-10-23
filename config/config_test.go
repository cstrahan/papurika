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

// SelectProfile exists so that anything reporting "which profile will be used"
// agrees with what Resolve actually uses. If these two ever drift, `papurika
// profiles` reports one profile while commands run against another.
func TestSelectProfileAgreesWithResolve(t *testing.T) {
	cfg := &Config{
		DefaultProfile: "prod",
		Profiles: map[string]Profile{
			"prod": {MasterURL: "https://prod:8000"},
			"lab":  {MasterURL: "https://lab:8000"},
			"main": {MasterURL: "https://main:8000"},
		},
	}
	for _, tc := range []struct {
		name     string
		explicit string
		env      string
		want     string
		source   string
	}{
		{"explicit wins", "lab", "prod", "lab", "explicit"},
		{"env beats default_profile", "", "lab", "lab", "env"},
		{"default_profile when no env", "", "", "prod", "default_profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAPURIKA_PROFILE", tc.env)

			sel := cfg.SelectProfile(tc.explicit)
			if sel.Name != tc.want || sel.Source != tc.source {
				t.Errorf("SelectProfile = %+v, want %s via %s", sel, tc.want, tc.source)
			}
			r, err := cfg.Resolve(tc.explicit, Overrides{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Profile != sel.Name {
				t.Errorf("Resolve used %q but SelectProfile reported %q", r.Profile, sel.Name)
			}
		})
	}

	// With nothing to go on, both land on "main".
	t.Run("fallback", func(t *testing.T) {
		t.Setenv("PAPURIKA_PROFILE", "")
		bare := &Config{Profiles: map[string]Profile{"main": {MasterURL: "https://m:8000"}}}
		sel := bare.SelectProfile("")
		if sel.Name != "main" || sel.Source != "fallback" || !sel.Exists {
			t.Errorf("SelectProfile = %+v", sel)
		}
		r, err := bare.Resolve("", Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		if r.Profile != sel.Name {
			t.Errorf("Resolve used %q but SelectProfile reported %q", r.Profile, sel.Name)
		}
	})
}
