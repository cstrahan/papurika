package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cstrahan/papurika/config"
)

func boolp(b bool) *bool { return &b }

func sampleConfig() *config.Config {
	return &config.Config{
		SourcePath:     "/etc/papurika/config.toml",
		DefaultProfile: "prod",
		Profiles: map[string]config.Profile{
			"prod": {
				MasterURL:       "https://salt.example.com:8000",
				User:            "me",
				Eauth:           "file",
				PasswordCommand: []string{"pass", "show", "salt/prod"},
			},
			"lab": {
				MasterURL: "http://localhost:8000",
				User:      "saltdev",
				Eauth:     "pam",
				Password:  "hunter2",
				VerifySSL: boolp(false),
				Timeout:   "30s",
			},
		},
	}
}

// The whole point of the command: it must be safe to log or pipe. No rendering
// path may ever emit a password value.
func TestProfilesNeverEmitTheSecret(t *testing.T) {
	t.Setenv("PAPURIKA_PROFILE", "")
	doc := buildProfilesDoc(sampleConfig(), "")

	var pretty bytes.Buffer
	renderProfiles(&pretty, doc, glyphs{})
	var warn bytes.Buffer
	warnProfiles(&warn, doc)
	asJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	for name, out := range map[string]string{
		"pretty": pretty.String(),
		"stderr": warn.String(),
		"json":   string(asJSON),
	} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("%s output leaked the password:\n%s", name, out)
		}
	}
	// ...while still saying a plaintext password is configured.
	if !strings.Contains(string(asJSON), `"plaintext":true`) {
		t.Error("json should flag the plaintext password without revealing it")
	}
	if !strings.Contains(warn.String(), `profile "lab" stores its password in plaintext`) {
		t.Errorf("expected a plaintext warning on stderr, got %q", warn.String())
	}
}

// The reported source must match the precedence config.Resolve actually applies,
// or the command misreports which credential will be used.
func TestSummarizeSecretMatchesResolvePrecedence(t *testing.T) {
	both := config.Profile{
		PasswordCommand: []string{"pass", "show", "x"},
		Password:        "plaintext-loses",
	}
	if got := summarizeSecret(both).Source; got != "password_command" {
		t.Errorf("source = %q, want password_command (it outranks plaintext in Resolve)", got)
	}
	if summarizeSecret(both).Plaintext {
		t.Error("plaintext must not be flagged when password_command wins")
	}
	if got := summarizeSecret(config.Profile{Password: "p"}).Source; got != "password" {
		t.Errorf("source = %q, want password", got)
	}
	if got := summarizeSecret(config.Profile{}).Source; got != "none" {
		t.Errorf("source = %q, want none", got)
	}
}

func TestProfilesDocShape(t *testing.T) {
	t.Setenv("PAPURIKA_PROFILE", "")
	doc := buildProfilesDoc(sampleConfig(), "")

	if doc.Schema != profilesSchema {
		t.Errorf("schema = %q, want %q", doc.Schema, profilesSchema)
	}
	if len(doc.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(doc.Profiles))
	}
	// Sorted by name, so automation gets a stable ordering.
	if doc.Profiles[0].Name != "lab" || doc.Profiles[1].Name != "prod" {
		t.Errorf("profiles not sorted: %s, %s", doc.Profiles[0].Name, doc.Profiles[1].Name)
	}
	if doc.Profiles[0].Default || !doc.Profiles[1].Default {
		t.Error("default flag should be on prod only")
	}
	// verify_ssl defaults to true when the file is silent, false when it says so.
	if !doc.Profiles[1].VerifySSL {
		t.Error("prod verify_ssl should default to true")
	}
	if doc.Profiles[0].VerifySSL {
		t.Error("lab verify_ssl should be false")
	}
}

func TestProfilesSelectionSources(t *testing.T) {
	cfg := sampleConfig()

	t.Run("explicit flag", func(t *testing.T) {
		t.Setenv("PAPURIKA_PROFILE", "")
		d := buildProfilesDoc(cfg, "lab")
		if d.Selected.Name != "lab" || d.Selected.Source != "explicit" || !d.Selected.Exists {
			t.Errorf("selected = %+v", d.Selected)
		}
	})
	t.Run("env", func(t *testing.T) {
		t.Setenv("PAPURIKA_PROFILE", "lab")
		d := buildProfilesDoc(cfg, "")
		if d.Selected.Name != "lab" || d.Selected.Source != "env" {
			t.Errorf("selected = %+v", d.Selected)
		}
	})
	t.Run("default_profile", func(t *testing.T) {
		t.Setenv("PAPURIKA_PROFILE", "")
		d := buildProfilesDoc(cfg, "")
		if d.Selected.Name != "prod" || d.Selected.Source != "default_profile" {
			t.Errorf("selected = %+v", d.Selected)
		}
	})
	// The surprising one: no default_profile means a profile named "main" that
	// probably doesn't exist. Say so rather than letting it fail later.
	t.Run("fallback to main", func(t *testing.T) {
		t.Setenv("PAPURIKA_PROFILE", "")
		d := buildProfilesDoc(&config.Config{Profiles: map[string]config.Profile{"a": {}}}, "")
		if d.Selected.Name != "main" || d.Selected.Source != "fallback" || d.Selected.Exists {
			t.Errorf("selected = %+v", d.Selected)
		}
		var warn bytes.Buffer
		warnProfiles(&warn, d)
		if !strings.Contains(warn.String(), "not defined") {
			t.Errorf("expected a warning that the selection is undefined, got %q", warn.String())
		}
	})
}

// A missing config file is not an error — a fully flag/env-driven invocation is
// valid — so the command must still produce usable output.
func TestProfilesWithNoConfigFile(t *testing.T) {
	t.Setenv("PAPURIKA_PROFILE", "")
	doc := buildProfilesDoc(&config.Config{}, "")

	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// Must be an empty array, not null: automation iterating it shouldn't crash.
	if !strings.Contains(string(b), `"profiles":[]`) {
		t.Errorf("want an empty profiles array, got %s", b)
	}
	var out bytes.Buffer
	renderProfiles(&out, doc, glyphs{})
	if !strings.Contains(out.String(), "no config file found") {
		t.Errorf("pretty output should say no config was found, got %q", out.String())
	}
	// No profiles means nothing to nag about.
	var warn bytes.Buffer
	warnProfiles(&warn, doc)
	if warn.Len() != 0 {
		t.Errorf("no warnings expected for an empty config, got %q", warn.String())
	}
}

func TestShellArgvQuotesOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"pass", "show", "salt/prod"}, "pass show salt/prod"},
		{[]string{"sh", "-c", "echo hi"}, `sh -c "echo hi"`},
		{[]string{"a", ""}, `a ""`},
	} {
		if got := shellArgv(tc.in); got != tc.want {
			t.Errorf("shellArgv(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
