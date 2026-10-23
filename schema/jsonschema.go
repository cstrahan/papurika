package schema

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// This file is the machine-readable half of the v1 contract: a description of
// every type and property, used both to emit a JSON Schema ([JSONSchema], and
// so `papurika schema`) and — cross-checked against the Go structs by
// reflection in the tests — to pin the field set.
//
// Why a hand-written descriptor rather than pure reflection: reflection knows
// the names and the Go types, and nothing that matters. It cannot say that
// `retcode` is null when a minion reported none and that this is different
// from zero, that `return` is arbitrary salt output, or what the known values
// of `status` are. Those are the parts a consumer actually needs, so they have
// to be written down. The cost is that adding a field to a struct without
// describing it here fails the build, which is the intended trade.

// v1Field describes one property of a v1 type — the parts reflection cannot
// know.
type v1Field struct {
	// tag is the exact json struct tag, ",omitempty" included. It is compared
	// against the Go struct field by reflection, so this descriptor cannot
	// drift from the types it claims to describe.
	tag  string
	desc string
	// typ is the JSON type(s). Empty means any JSON value — used for the two
	// properties that carry salt's own output verbatim.
	typ      []string
	itemType string // element type, for an array of scalars
	itemRef  string // element definition, for an array of objects
	ref      string // definition, for an object-valued property
	constant any    // const — only for the version identifier
	// examples lists the known values of an OPEN vocabulary. Deliberately not
	// `enum`: v1 may gain a status, outcome or error kind (it gained the
	// "pending" status and outcome in this very version), and an enum would
	// make a conforming future document fail validation against this schema.
	examples []any
}

// name is the property name, without the omitempty suffix.
func (f v1Field) name() string {
	if i := strings.IndexByte(f.tag, ','); i >= 0 {
		return f.tag[:i]
	}
	return f.tag
}

// required mirrors the Go tag exactly: a property is required precisely when
// it has no omitempty, because that is when the encoder always writes it. So
// `retcode` on a minion is required-and-nullable, which is the whole point of
// it not being omitempty.
func (f v1Field) required() bool { return !strings.Contains(f.tag, ",omitempty") }

// v1Object describes one named type in the schema.
type v1Object struct {
	name   string
	desc   string
	goType any // the Go struct this describes; reflected over in the tests
	fields []v1Field
}

func strs(v ...string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// v1Objects is the complete description of papurika.dev/result/v1. The first
// entry is the document root.
var v1Objects = []v1Object{
	{
		name:   "Result",
		desc:   "One papurika result document.",
		goType: Result{},
		fields: []v1Field{
			{tag: "schema", desc: "Schema identifier. Check this before trusting the rest of the document.",
				typ: []string{"string"}, constant: Version},
			{tag: "summary", desc: "Every minion of every command, folded into one set of counts.",
				typ: []string{"object"}, ref: "Summary"},
			{tag: "commands", desc: "One entry per command in the request, in order. Never null; an empty document carries an empty array.",
				typ: []string{"array"}, itemRef: "Command"},
			{tag: "error,omitempty", desc: "Set when the request itself failed. Present both on a document that gathered nothing (summary.outcome is \"error\") and on a partial one that was cut short, where the outcome describes what was gathered and this says what stopped it.",
				typ: []string{"object"}, ref: "Error"},
		},
	},
	{
		name:   "Summary",
		desc:   "Aggregate counts across every command. targeted = returned + noresponse + pending; returned = succeeded + failed.",
		goType: Summary{},
		fields: []v1Field{
			{tag: "outcome", desc: "Overall outcome. Open vocabulary — branch on the values you handle and keep a default.",
				typ:      []string{"string"},
				examples: strs(OutcomeOK, OutcomePartial, OutcomeFailed, OutcomePending, OutcomeNoMatch, OutcomeError)},
			{tag: "jids,omitempty", desc: "Every job id the run touched, including each chunk of a batched run. These stay recoverable with `papurika watch <jid>` whatever else happened.",
				typ: []string{"array"}, itemType: "string"},
			{tag: "targeted", desc: "Minions the command(s) were dispatched to. 0 for a master-side (runner/wheel) command.", typ: []string{"integer"}},
			{tag: "returned", desc: "Minions that answered, whether or not the command succeeded.", typ: []string{"integer"}},
			{tag: "succeeded", desc: "Minions that answered with a zero or absent retcode.", typ: []string{"integer"}},
			{tag: "failed", desc: "Minions that answered with a non-zero retcode or a per-minion error.", typ: []string{"integer"}},
			{tag: "noresponse", desc: "Minions probed with saltutil.find_job and found not running the job.", typ: []string{"integer"}},
			{tag: "pending", desc: "Minions with no terminal outcome yet. Distinct from noresponse: nothing has been concluded about them.", typ: []string{"integer"}},
		},
	},
	{
		name:   "Command",
		desc:   "One command's outcome. A minion-targeting command fills minions; a master-side (runner/wheel) command fills value instead and leaves minions absent. targeted == 0 distinguishes them.",
		goType: Command{},
		fields: []v1Field{
			{tag: "index", desc: "Position of this command in the request.", typ: []string{"integer"}},
			{tag: "client", desc: "The salt-api client that ran it (local, local_async, local_batch, runner, wheel, ssh, ...).", typ: []string{"string"}},
			{tag: "fun", desc: "The salt function that was run.", typ: []string{"string"}},
			{tag: "jid,omitempty", desc: "Job id, when the command produced one. Absent for synchronous clients, which salt-api does not give a jid.", typ: []string{"string"}},
			{tag: "extra_jids,omitempty", desc: "Remaining job ids when one command covers several jobs — a client-side-batched run, where each chunk is its own job merged into this view. Use summary.jids to get them all without special-casing.",
				typ: []string{"array"}, itemType: "string"},
			{tag: "targeted,omitempty", desc: "Minion ids the job was dispatched to, as salt reported them.",
				typ: []string{"array"}, itemType: "string"},
			{tag: "minions,omitempty", desc: "Per-minion outcomes, sorted by id. Absent for a master-side command.",
				typ: []string{"array"}, itemRef: "Minion"},
			{tag: "value,omitempty", desc: "Return of a master-side (runner/wheel) command, verbatim. Any JSON."},
			{tag: "retcode,omitempty", desc: "Command-level return code, when salt reported one.", typ: []string{"integer"}},
		},
	},
	{
		name:   "Minion",
		desc:   "One minion's outcome. A command that ran and failed is status \"responded\" with a non-zero retcode — failure is not a separate status, so \"the minion answered\" and \"the thing it ran worked\" stay separate questions.",
		goType: Minion{},
		fields: []v1Field{
			{tag: "id", desc: "Minion id.", typ: []string{"string"}},
			{tag: "status", desc: "Whether and how the minion answered. Open vocabulary. \"noresponse\" is a claim (probed and found absent); \"pending\" is the absence of one (has not answered yet) — treating pending as failure reports a healthy in-flight job as broken.",
				typ:      []string{"string"},
				examples: strs(StatusResponded, StatusNoResponse, StatusPending, StatusError, StatusNotMatched)},
			{tag: "retcode", desc: "Return code, or null when the minion reported none. Null and 0 are different facts, which is why this property is always present rather than omitted.",
				typ: []string{"integer", "null"}},
			{tag: "return", desc: "The minion's return value, exactly as salt sent it. Any JSON — shape depends on the function; a state run returns a map of state id to result, or an array of error strings if the run never started."},
		},
	},
	{
		name:   "Error",
		desc:   "A request-level failure: authentication, transport, a malformed request, or the clock running out. Per-minion outcomes are never reported here — those are a minion status plus a retcode.",
		goType: Error{},
		fields: []v1Field{
			{tag: "kind", desc: "Failure class. Open vocabulary; \"error\" is the catch-all.",
				typ: []string{"string"},
				examples: strs(ErrKindTimeout, ErrKindInterrupted, ErrKindAuthFailure, ErrKindAuthDenied,
					ErrKindMalformed, ErrKindServerError, ErrKindJobNotFound, ErrKindNotFound,
					ErrKindUnsupported, ErrKindTransport, ErrKindOther)},
			{tag: "message", desc: "Human-readable description. Not a stable string; branch on kind, or on the process exit code.", typ: []string{"string"}},
		},
	},
}

// --- emission ---

// jsSchema is the subset of JSON Schema draft 2020-12 this emits.
type jsSchema struct {
	Dialect     string               `json:"$schema,omitempty"`
	ID          string               `json:"$id,omitempty"`
	Title       string               `json:"title,omitempty"`
	Description string               `json:"description,omitempty"`
	Ref         string               `json:"$ref,omitempty"`
	Type        any                  `json:"type,omitempty"`
	Const       any                  `json:"const,omitempty"`
	Examples    []any                `json:"examples,omitempty"`
	Items       *jsSchema            `json:"items,omitempty"`
	Properties  map[string]*jsSchema `json:"properties,omitempty"`
	Required    []string             `json:"required,omitempty"`
	Defs        map[string]*jsSchema `json:"$defs,omitempty"`
}

// SchemaID is the JSON Schema $id of the result document. It is the absolute
// form of [Version], which is the bare identifier carried in a document's
// "schema" property; JSON Schema wants a URI there, a document does not.
const SchemaID = "https://papurika.dev/result/v1"

// jsonSchemaDialect is the JSON Schema draft this conforms to.
const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

func (f v1Field) schema() *jsSchema {
	s := &jsSchema{Description: f.desc, Const: f.constant, Examples: f.examples}
	switch {
	case f.ref != "":
		// A $ref alongside other keywords is legal in 2020-12, so the
		// description stays attached to the property that has it.
		s.Ref = "#/$defs/" + f.ref
	case f.itemRef != "":
		s.Type = "array"
		s.Items = &jsSchema{Ref: "#/$defs/" + f.itemRef}
	case f.itemType != "":
		s.Type = "array"
		s.Items = &jsSchema{Type: f.itemType}
	case len(f.typ) == 1:
		s.Type = f.typ[0]
	case len(f.typ) > 1:
		s.Type = f.typ
		// len(f.typ) == 0: any JSON value, so no type constraint at all.
	}
	return s
}

func (o v1Object) schema() *jsSchema {
	s := &jsSchema{
		Type:        "object",
		Title:       o.name,
		Description: o.desc,
		Properties:  map[string]*jsSchema{},
	}
	for _, f := range o.fields {
		s.Properties[f.name()] = f.schema()
		if f.required() {
			s.Required = append(s.Required, f.name())
		}
	}
	// additionalProperties is deliberately left unset (i.e. permitted): v1 may
	// GAIN fields, and a consumer validating newer output against this schema
	// must not fail for that reason alone.
	return s
}

// buildJSONSchema renders the descriptor above as a JSON Schema document.
func buildJSONSchema() string {
	root := &jsSchema{
		Dialect:     jsonSchemaDialect,
		ID:          SchemaID,
		Title:       "papurika result document",
		Description: "The JSON document papurika's --output json|yaml emits. Within this version, properties may be ADDED and the open vocabularies may gain values, so a consumer must ignore what it does not recognize; nothing is renamed, removed, or redefined. --output ndjson is a different, streaming shape and is not described here.",
		Ref:         "#/$defs/" + v1Objects[0].name,
		Defs:        map[string]*jsSchema{},
	}
	for _, o := range v1Objects {
		root.Defs[o.name] = o.schema()
	}
	// Not json.MarshalIndent: that escapes <, > and & for embedding in HTML,
	// which here only turns `papurika watch <jid>` in a description into
	// `papurika watch \u003cjid\u003e`. An Encoder can turn it off, and
	// already terminates with a newline.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		// Unreachable: every type above is a plain struct of marshalable
		// values, and a test renders it on every run.
		panic("schema: cannot render JSON Schema: " + err.Error())
	}
	return buf.String()
}

// JSONSchema returns the JSON Schema (draft 2020-12) describing
// papurika.dev/result/v1, as pretty-printed JSON ending in a newline.
//
// The CLI exposes it as `papurika schema`, so an installed binary describes
// its own output without anyone having to find the docs. The result is
// computed once and shared; callers must not modify the returned string, which
// Go makes impossible anyway.
var JSONSchema = sync.OnceValue(buildJSONSchema)
