package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Stamped by the release build (GoReleaser ldflags). When papurika is built
// with `go install`, these stay at their defaults and buildInfo() falls back to
// the module version the Go toolchain embeds.
var (
	version = ""
	commit  = ""
	date    = ""
)

// buildInfo reports the version, commit and build date, preferring the
// ldflags-stamped values and falling back to what the Go toolchain embeds.
// vcs.* settings describe the *local* tree, so they only fill gaps the release
// build left empty — a stamped commit is never rewritten as dirty.
func buildInfo() (ver, rev, built string) {
	ver, rev, built = version, commit, date
	stamped := rev != ""
	dirty := false
	if bi, ok := debug.ReadBuildInfo(); ok {
		if ver == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			ver = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if rev == "" {
					rev = s.Value
				}
			case "vcs.time":
				if built == "" {
					built = s.Value
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty && !stamped && rev != "" {
		rev += "-dirty"
	}
	if ver == "" {
		ver = "devel"
	}
	return ver, rev, built
}

// versionString is the one-line form used by `papurika version` and --version.
func versionString() string {
	ver, rev, built := buildInfo()
	s := "papurika " + ver
	if rev != "" {
		s += " (" + rev + ")"
	}
	if built != "" {
		s += " built " + built
	}
	return s + fmt.Sprintf("\n%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the papurika version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), versionString())
			return nil
		},
	}
}
