package config

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SecretSource yields a password on demand, resolved lazily so secrets aren't
// held in memory longer than needed.
type SecretSource interface {
	Secret(ctx context.Context) (string, error)
}

// StaticSecret is a literal password (the discouraged plaintext path).
type StaticSecret string

func (s StaticSecret) Secret(context.Context) (string, error) { return string(s), nil }

// CommandSecret runs an argv and uses the first line of its stdout (trimmed) as
// the password — à la git's credential.helper or mbsync's PassCmd. The command
// may pull from a password manager, the OS keychain, a GPG-encrypted file, etc.,
// and may prompt or require a biometric/touch assertion (it inherits stdin/stderr
// so an interactive helper works).
type CommandSecret struct {
	Argv []string
}

func (c CommandSecret) Secret(ctx context.Context) (string, error) {
	if len(c.Argv) == 0 {
		return "", fmt.Errorf("config: empty password_command")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	// Inherit stderr so a prompt/diagnostic from the helper reaches the user; the
	// helper may also read the controlling TTY for an interactive unlock.
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("config: password_command %q failed: %w", c.Argv[0], err)
	}
	secret := out.String()
	if i := strings.IndexByte(secret, '\n'); i >= 0 {
		secret = secret[:i]
	}
	secret = strings.TrimRight(secret, "\r")
	if secret == "" {
		return "", fmt.Errorf("config: password_command produced no output")
	}
	return secret, nil
}
