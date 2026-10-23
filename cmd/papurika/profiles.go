package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/cstrahan/papurika/config"
	"github.com/spf13/cobra"
	sigsyaml "sigs.k8s.io/yaml"
)

// profilesSchema versions the machine-readable shape, as the result schema does.
const profilesSchema = "papurika.dev/profiles/v1"

// profilesDoc is the whole rendered answer to "what can this install connect to?".
type profilesDoc struct {
	Schema         string           `json:"schema"`
	ConfigPath     string           `json:"config_path,omitempty"`
	DefaultProfile string           `json:"default_profile,omitempty"`
	Selected       selectedProfile  `json:"selected"`
	Profiles       []profileSummary `json:"profiles"`
}

// selectedProfile is which profile this invocation would actually use. Source
// explains why, because the fallback case surprises people.
type selectedProfile struct {
	Name   string `json:"name"`
	Source string `json:"source"` // explicit | env | default_profile | fallback
	Exists bool   `json:"exists"`
}

type profileSummary struct {
	Name      string      `json:"name"`
	Default   bool        `json:"default"`
	MasterURL string      `json:"master_url,omitempty"`
	User      string      `json:"user,omitempty"`
	Eauth     string      `json:"eauth,omitempty"`
	VerifySSL bool        `json:"verify_ssl"`
	CACert    string      `json:"ca_cert,omitempty"`
	Timeout   string      `json:"timeout,omitempty"`
	Secret    secretSumry `json:"secret"`
}

// secretSumry describes where a profile's password comes from — never what it
// is. There is deliberately no field that could hold the secret itself.
type secretSumry struct {
	// Source is "password_command", "password" (plaintext in the file),
	// or "none" (papurika will prompt, or PAPURIKA_PASSWORD must be set).
	Source string `json:"source"`
	// Command is the password_command argv. Shown because its whole purpose is
	// to be inspectable — it names where the secret lives, it is not the
	// secret. Do not embed a literal password in it; that defeats the field.
	Command []string `json:"command,omitempty"`
	// Plaintext flags a password stored directly in the config file. The value
	// is never included.
	Plaintext bool `json:"plaintext,omitempty"`
}

func newProfilesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "List configured profiles (secrets redacted)",
		Long: "List the profiles in the config file, which one this invocation would use, " +
			"and where each profile's password comes from.\n\n" +
			"No password value is ever printed, so the output is safe to log or pipe. " +
			"Reads only the config file — no network, no credentials needed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPaths()...)
			if err != nil {
				return err
			}
			doc := buildProfilesDoc(cfg, flagProfile)
			switch resolveOutput() {
			case outputJSON, outputNDJSON, outputRaw:
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(doc)
			case outputYAML:
				b, err := json.Marshal(doc)
				if err != nil {
					return err
				}
				y, err := sigsyaml.JSONToYAML(b)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(y)
				return err
			default:
				renderProfiles(cmd.OutOrStdout(), doc, glyphs{color: useColor()})
				warnProfiles(os.Stderr, doc)
				return nil
			}
		},
	}
}

func buildProfilesDoc(cfg *config.Config, explicit string) profilesDoc {
	sel := cfg.SelectProfile(explicit)
	doc := profilesDoc{
		Schema:         profilesSchema,
		ConfigPath:     cfg.SourcePath,
		DefaultProfile: cfg.DefaultProfile,
		Selected:       selectedProfile{Name: sel.Name, Source: sel.Source, Exists: sel.Exists},
		Profiles:       []profileSummary{},
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Profiles[n]
		verify := true
		if p.VerifySSL != nil {
			verify = *p.VerifySSL
		}
		doc.Profiles = append(doc.Profiles, profileSummary{
			Name:      n,
			Default:   n == cfg.DefaultProfile,
			MasterURL: p.MasterURL,
			User:      p.User,
			Eauth:     p.Eauth,
			VerifySSL: verify,
			CACert:    p.CACert,
			Timeout:   p.Timeout,
			Secret:    summarizeSecret(p),
		})
	}
	return doc
}

// summarizeSecret mirrors config.Resolve's file-level precedence
// (password_command before plaintext password) so the reported source is the
// one that would actually be used.
//
// It cannot report PAPURIKA_PASSWORD or --password: those outrank the file but
// are properties of an invocation, not of a profile.
func summarizeSecret(p config.Profile) secretSumry {
	switch {
	case len(p.PasswordCommand) > 0:
		return secretSumry{Source: "password_command", Command: p.PasswordCommand}
	case p.Password != "":
		return secretSumry{Source: "password", Plaintext: true}
	default:
		return secretSumry{Source: "none"}
	}
}

func renderProfiles(w io.Writer, doc profilesDoc, g glyphs) {
	if doc.ConfigPath != "" {
		fmt.Fprintf(w, "%s %s\n\n", g.dim("config:"), doc.ConfigPath)
	} else {
		fmt.Fprintf(w, "%s\n\n", g.dim("config: (no config file found)"))
	}

	if len(doc.Profiles) == 0 {
		fmt.Fprintf(w, "%s\n", g.dim("no profiles defined"))
		return
	}

	width := 0
	for _, p := range doc.Profiles {
		if len(p.Name) > width {
			width = len(p.Name)
		}
	}
	// Continuation lines line up under the URL column.
	indent := spaces(width + 5)
	for _, p := range doc.Profiles {
		marker := " "
		if p.Default {
			marker = "*"
		}
		name := p.Name
		if p.Name == doc.Selected.Name {
			name = g.cyan(p.Name)
		}
		// Pad by the plain name's width: color codes add bytes but no columns.
		fmt.Fprintf(w, " %s %s%s  %s\n", marker, name, spaces(width-len(p.Name)), p.MasterURL)

		detail := fmt.Sprintf("user=%s eauth=%s", orDash(p.User), orDash(p.Eauth))
		if p.CACert != "" {
			detail += " ca_cert=" + p.CACert
		}
		if p.Timeout != "" {
			detail += " timeout=" + p.Timeout
		}
		line := g.dim(detail)
		if !p.VerifySSL {
			line += " " + g.yellow("verify_ssl=false")
		}
		fmt.Fprintf(w, "%s%s\n", indent, line)
		fmt.Fprintf(w, "%s%s\n", indent, secretLine(p.Secret, g))
	}

	fmt.Fprintln(w)
	if doc.DefaultProfile != "" {
		fmt.Fprintf(w, "%s\n", g.dim("* = default_profile"))
	}
	fmt.Fprintf(w, "%s\n", g.dim(selectionLine(doc)))
}

func secretLine(s secretSumry, g glyphs) string {
	switch s.Source {
	case "password_command":
		return g.dim("secret: password_command → " + shellArgv(s.Command))
	case "password":
		return g.yellow("secret: plaintext password in the config file") +
			g.dim(" (value hidden; prefer password_command)")
	default:
		return g.dim("secret: none (PAPURIKA_PASSWORD or a prompt)")
	}
}

func selectionLine(doc profilesDoc) string {
	s := doc.Selected
	why := map[string]string{
		"explicit":        "selected by --profile",
		"env":             "selected by PAPURIKA_PROFILE",
		"default_profile": "selected by default_profile",
		"fallback":        "nothing selects one, so using the conventional name",
	}[s.Source]
	line := fmt.Sprintf("→ this invocation would use %q (%s)", s.Name, why)
	if !s.Exists {
		line += " — which is not defined"
	}
	return line
}

// warnProfiles writes advisories to stderr, keeping stdout pure data. The JSON
// form carries the same facts (secret.plaintext, selected.exists), so automation
// does not need to parse these.
func warnProfiles(w io.Writer, doc profilesDoc) {
	for _, p := range doc.Profiles {
		if p.Secret.Plaintext {
			fmt.Fprintf(w, "papurika: profile %q stores its password in plaintext; prefer password_command\n", p.Name)
		}
	}
	if !doc.Selected.Exists && len(doc.Profiles) > 0 {
		fmt.Fprintf(w, "papurika: profile %q is not defined; pass --profile or set default_profile\n", doc.Selected.Name)
	}
}

// shellArgv renders an argv readably, quoting only the elements that need it so
// the common case stays easy to read.
func shellArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$`") {
			parts[i] = strconv.Quote(a)
		} else {
			parts[i] = a
		}
	}
	return strings.Join(parts, " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func spaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

func newServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Show what this endpoint is: netapi module, salt version, enabled clients",
		Long: "Report which salt-api the current profile points at — rest_cherrypy or " +
			"rest_tornado, the master's salt version, and which netapi client types it " +
			"has enabled.\n\n" +
			"Behavior differs enough between salt versions and between the two netapi " +
			"modules that this is worth being able to check directly, particularly " +
			"across masters that are not all on the same release.\n\n" +
			"Everything but the salt version comes from an unauthenticated request, so " +
			"it works before credentials are set up; the version needs auth and is " +
			"reported as missing rather than failing the command.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := buildClient()
			if err != nil {
				return err
			}
			info, ierr := cl.ServerInfo(cmd.Context())
			if ierr != nil {
				return emit(nil, ierr)
			}
			switch resolveOutput() {
			case outputPretty:
				g := glyphs{color: useColor()}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", info.URL)
				fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", g.dim("netapi: "), info.Module)
				if info.Server != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", g.dim("server: "), info.Server)
				}
				if info.SaltVersion != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", g.dim("salt:   "), info.SaltVersion)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", g.dim("salt:   "),
						g.yellow("unknown")+g.dim(" ("+info.VersionErr+")"))
				}
				if len(info.Clients) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", g.dim("clients:"), strings.Join(info.Clients, " "))
				}
			case outputYAML:
				b, err := json.Marshal(info)
				if err != nil {
					return err
				}
				y, err := sigsyaml.JSONToYAML(b)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(y)
				return err
			default:
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			return nil
		},
	}
}
