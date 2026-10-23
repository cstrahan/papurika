package main

import (
	"fmt"

	"github.com/cstrahan/papurika/schema"
	"github.com/spf13/cobra"
	sigsyaml "sigs.k8s.io/yaml"
)

// newSchemaCmd prints the JSON Schema for papurika's own result documents, so
// an installed binary describes its own output without anyone having to find
// the docs — and so the description a consumer validates against is the one
// that binary actually emits, rather than whatever a wiki says.
//
// It touches no network and needs no credentials, like `papurika profiles`.
func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema for papurika's --output json|yaml documents",
		Long: "Print the JSON Schema (draft 2020-12) describing papurika.dev/result/v1, " +
			"the document --output json and --output yaml emit.\n\n" +
			"Needs no network and no credentials. Pipe it to a validator, vendor it " +
			"next to a script, or read it to find out what a field means:\n\n" +
			"  papurika schema > result-v1.schema.json\n" +
			"  papurika run '*' test.ping -o json | check-jsonschema --schemafile result-v1.schema.json -\n\n" +
			"Compatibility: within v1 properties may be ADDED and the open vocabularies " +
			"(status, outcome, error kind) may gain values, so a consumer must ignore what " +
			"it does not recognize. Nothing is renamed, removed or redefined — that would " +
			"be v2. The schema therefore permits unknown properties and describes the known " +
			"vocabulary values as `examples` rather than `enum`, so a document from a newer " +
			"papurika still validates against an older copy of this schema.\n\n" +
			"--output ndjson is a different, streaming shape and is NOT described here.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if resolveOutput() == outputYAML {
				y, err := sigsyaml.JSONToYAML([]byte(schema.JSONSchema()))
				if err != nil {
					return err
				}
				_, _ = out.Write(y)
				return &exitErr{code: exitOK}
			}
			// Every other mode prints the schema itself: it IS the artifact,
			// and a pretty rendering of a JSON Schema is just the JSON Schema.
			_, _ = fmt.Fprint(out, schema.JSONSchema())
			return &exitErr{code: exitOK}
		},
	}
}
