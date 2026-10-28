// SPDX-License-Identifier: Apache-2.0

package projection

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"github.com/getkin/kin-openapi/openapi3"
)

// Conditionals in a hidden helper are translated when they have a supported
// shape, and dropped and reported like any other conditional when they don't.
func TestPrepareTranslatesHelperConditionals(t *testing.T) {
	tests := []struct {
		name        string
		conditional string
		want        *helperRule // nil: dropped and reported
	}{
		{
			name:        "absent subject",
			conditional: `if a == _|_ { f: #T }`,
			want:        &helperRule{subject: "a", absent: true, field: "f", def: "T"},
		},
		{
			name:        "excluded values",
			conditional: `if k != "x" && k != "y" { f: #T }`,
			want:        &helperRule{subject: "k", values: []string{"x", "y"}, field: "f", def: "T"},
		},
		{
			name:        "non-empty list body",
			conditional: `if k != "x" { l: [#T, ...#T] }`,
			want:        &helperRule{subject: "k", values: []string{"x"}, field: "l", def: "T", list: true},
		},
		{name: "equality to a value", conditional: `if k == "x" { f: #T }`},
		{name: "bytes literal", conditional: `if k != 'x' { f: #T }`},
		{name: "optional body field", conditional: `if k != "x" { f?: #T }`},
		{name: "hidden body field", conditional: `if k != "x" { _f: #T }`},
		{name: "two body fields", conditional: `if k != "x" { f: #T, l: [#T, ...#T] }`},
		{name: "narrowing body", conditional: `if k != "x" { f: "fixed" }`},
		{name: "two subjects", conditional: `if k != "x" && a != "y" { f: #T }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, info := mustPrepare(t, `package p
#T: string
#B: {k: string, a?: string, f?: #T, l?: [#T, ...#T]}
#_BStrict: {
	@go(-)
} & #B & {
	k:  string
	a?: string
	`+tt.conditional+`
}
`)
			rules := info.HelperRules["_BStrict"]
			if tt.want == nil {
				if len(rules) != 0 || len(info.DroppedConditionals) != 1 {
					t.Fatalf("want the conditional dropped and reported; rules = %+v, dropped = %v", rules, info.DroppedConditionals)
				}
				return
			}
			if len(rules) != 1 || len(info.DroppedConditionals) != 0 {
				t.Fatalf("want one rule and nothing dropped; rules = %+v, dropped = %v", rules, info.DroppedConditionals)
			}
			got := rules[0]
			got.pos = ""
			if !reflect.DeepEqual(got, *tt.want) {
				t.Errorf("rule = %+v, want %+v", got, *tt.want)
			}
		})
	}
}

// A condition must refer to the helper's own field. CUE resolves it lexically,
// so when the helper block does not declare the field under that identifier,
// or the conditional sits outside the helper's own conjuncts, it is dropped.
func TestPrepareDropsHelperConditionalsOutsideTheHelperScope(t *testing.T) {
	tests := map[string]string{
		"undeclared subject": `
	if k != "x" { f: #T }`,
		"quoted subject label": `
	"k": string
	if k != "x" { f: #T }`,
		"inside a let": `
	let extra = {
		k: string
		if k != "x" { f: #T }
	}
	_x: extra`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, info := mustPrepare(t, `package p
#T: string
k: "outside"
#B: {k: string, f?: #T}
#_BStrict: {
	@go(-)
} & #B & {`+body+`
}
`)
			if len(info.HelperRules) != 0 || len(info.DroppedConditionals) != 1 {
				t.Fatalf("rules = %+v, dropped = %v; want the conditional dropped", info.HelperRules, info.DroppedConditionals)
			}
		})
	}
}

// Only a hidden helper's own conditionals are translated; the same shape in an
// ordinary definition still has nowhere to go and is reported as dropped.
func TestPrepareLeavesOrdinaryConditionalsDropped(t *testing.T) {
	_, info := mustPrepare(t, `package p
#T: string
#B: {
	k: string
	f?: #T
	if k != "x" { f: #T }
}
`)
	if len(info.HelperRules) != 0 || len(info.DroppedConditionals) != 1 {
		t.Fatalf("rules = %+v, dropped = %v; want the conditional dropped", info.HelperRules, info.DroppedConditionals)
	}
}

const helperRuleSchema = `@gemara(status="stable")

package p

#Datetime: string
#Ref: {id: string}
#Result: "Passed" | "Failed" | "Not Run"

#Log: {
	result:   #Result
	start?:   #Datetime
	payload?: _
	source?:  #Ref
}

#_LogStrict: {
	@go(-)
} & #Log & {
	result:   #Result
	payload?: _
	if result != "Not Run" {
		start: #Datetime
	}
	if payload == _|_ {
		source: #Ref
	}
}

#Kind: "link" | "none"

#Pair: {
	kind:     #Kind
	targets?: [#Ref, ...#Ref]
}

#_PairStrict: {
	@go(-)
} & #Pair & {
	kind: #Kind
	if kind != "none" {
		targets: [#Ref, ...#Ref]
	}
}

#Doc: {
	logs?:  [#_LogStrict, ...#_LogStrict]
	pairs?: [#_PairStrict, ...#_PairStrict]
	plain?: [#Log, ...#Log]
}
`

// The projected rules must accept exactly what the CUE accepts: each case is
// checked against both, so a mismatch in either direction fails.
func TestHelperRulesMatchTheCUE(t *testing.T) {
	doc, err := convertSource(t, helperRuleSchema)
	if err != nil {
		t.Fatal(err)
	}
	schema := reloaded(t, doc).Components.Schemas["Doc"]
	if schema == nil {
		t.Fatal("Doc is missing from the generated document")
	}
	ctx := cuecontext.New()
	def := ctx.CompileString(helperRuleSchema).LookupPath(cue.ParsePath("#Doc"))
	if err := def.Err(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		doc   map[string]any
		valid bool
	}{
		{"executed with start and source", map[string]any{"logs": []any{map[string]any{"result": "Passed", "start": "t", "source": map[string]any{"id": "s"}}}}, true},
		{"executed without start", map[string]any{"logs": []any{map[string]any{"result": "Passed", "source": map[string]any{"id": "s"}}}}, false},
		{"not run without start", map[string]any{"logs": []any{map[string]any{"result": "Not Run", "source": map[string]any{"id": "s"}}}}, true},
		{"neither payload nor source", map[string]any{"logs": []any{map[string]any{"result": "Not Run"}}}, false},
		{"payload without source", map[string]any{"logs": []any{map[string]any{"result": "Not Run", "payload": "p"}}}, true},
		{"link without targets", map[string]any{"pairs": []any{map[string]any{"kind": "link"}}}, false},
		{"link with targets", map[string]any{"pairs": []any{map[string]any{"kind": "link", "targets": []any{map[string]any{"id": "t"}}}}}, true},
		{"none without targets", map[string]any{"pairs": []any{map[string]any{"kind": "none"}}}, true},
		// #Log is not strict in CUE, so the base schema must not gain the rules.
		{"base type stays lenient", map[string]any{"plain": []any{map[string]any{"result": "Passed"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cueErr := def.Unify(ctx.Encode(tt.doc)).Validate(cue.Concrete(true))
			if (cueErr == nil) != tt.valid {
				t.Fatalf("CUE valid = %v, want %v (%v); the case itself is wrong", cueErr == nil, tt.valid, cueErr)
			}
			oaErr := schema.Value.VisitJSON(tt.doc)
			if (oaErr == nil) != tt.valid {
				t.Errorf("OpenAPI valid = %v, CUE valid = %v:\n%v", oaErr == nil, tt.valid, oaErr)
			}
		})
	}
}

// A rule that cannot be projected faithfully is left out and reported, and
// generation carries on, as it did when every conditional was dropped.
func TestHelperRulesSkipUnfaithfulProjections(t *testing.T) {
	tests := []struct {
		name, base, helper, wantReason string
	}{
		{
			name:       "optional subject compared by value",
			base:       `k?: #K, f?: #T`,
			helper:     `k?: #K` + "\n\t" + `if k != "x" { f: #T }`,
			wantReason: "k is optional in #B",
		},
		{
			name:       "subject with a default",
			base:       `k: *"x" | "y" @gemara(default="x"), f?: #T`,
			helper:     `k: *"x" | "y"` + "\n\t" + `if k != "y" { f: #T }`,
			wantReason: "k has a default in #B",
		},
		{
			name:       "body narrows the base field",
			base:       `k: #K, f?: string`,
			helper:     `k: #K` + "\n\t" + `if k != "x" { f: #T }`,
			wantReason: "#B declares f differently",
		},
		{
			name:       "nullable base field",
			base:       `k: #K, f?: #T | null`,
			helper:     `k: #K` + "\n\t" + `if k != "x" { f: #T }`,
			wantReason: "#B declares f differently",
		},
		{
			name:       "field the base does not declare",
			base:       `k: #K, g?: string`,
			helper:     `k: #K` + "\n\t" + `if k != "x" { f: #T }`,
			wantReason: "#B does not declare f",
		},
		{
			name:       "body type with a default",
			base:       `k: #K, f?: #D`,
			helper:     `k: #K` + "\n\t" + `if k != "x" { f: #D }`,
			wantReason: "#D is satisfied without data",
		},
		{
			name:       "body type with only optional fields",
			base:       `k: #K, f?: #S`,
			helper:     `k: #K` + "\n\t" + `if k != "x" { f: #S }`,
			wantReason: "#S is satisfied without data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `@gemara(status="stable")

package p

#T: {id: string}
#K: "x" | "y"
#D: *"d" | "e"
#S: {note?: string}
#B: {` + tt.base + `}
#_BStrict: {
	@go(-)
} & #B & {
	` + tt.helper + `
}
#Doc: {bs?: [#_BStrict, ...#_BStrict]}
`
			dir := writeSourceModule(t, src)
			v, info, _, err := loadPrepared(dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := generateRaw(v, "test", "test")
			if err != nil {
				t.Fatal(err)
			}
			skipped, err := applyHelperRulesTyped(raw, info.HelperRules, v)
			if err != nil {
				t.Fatal(err)
			}
			if len(skipped) != 1 || !strings.Contains(skipped[0], tt.wantReason) {
				t.Fatalf("skipped = %q, want one note containing %q", skipped, tt.wantReason)
			}

			doc, err := convertSource(t, src)
			if err != nil {
				t.Fatalf("generation must not fail on a rule it cannot project: %v", err)
			}
			items, err := doc.Components.Schemas["Doc"].Value.Properties["bs"].Value.Items.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(items), "anyOf") {
				t.Errorf("the skipped rule was projected anyway: %s", items)
			}
		})
	}
}

// The repository's bad fixtures for the three strict helpers must now be
// rejected by the generated document too, and only for the rule's field.
func TestStrictHelperBadFixturesFailTheDocument(t *testing.T) {
	out := filepath.Join(t.TempDir(), "projection.yaml")
	writeGenerated(t, out, "", "")
	doc, err := openapi3.NewLoader().LoadFromFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// A new required field on #AssessmentLog, #Evidence or #Mapping also has to
	// be added to these fixtures, or they fail here for the wrong reason.
	tests := []struct {
		file, schema string
		fix          func(*testing.T, map[string]any) // adds what the rule asks for
	}{
		{"test/test-data/bad-evaluation-log-missing-start.yaml", "EvaluationLog", func(t *testing.T, d map[string]any) {
			evaluation := first(t, d["evaluations"], "evaluations")
			first(t, evaluation["assessment-logs"], "assessment-logs")["start"] = "2025-08-22T16:01:00Z"
		}},
		{"test/test-data/bad-audit-log-evidence-neither.yaml", "AuditLog", func(t *testing.T, d map[string]any) {
			for _, r := range sliceOf(d["results"]) {
				for _, e := range sliceOf(mapOf(r)["evidence"]) {
					mapOf(e)["payload"] = "inline evidence"
				}
			}
		}},
		{"test/test-data/bad-mapping-no-target.yaml", "MappingDocument", func(t *testing.T, d map[string]any) {
			first(t, d["mappings"], "mappings")["targets"] = []any{map[string]any{"entry-id": "T1"}}
		}},
	}
	for _, tt := range tests {
		t.Run(filepath.Base(tt.file), func(t *testing.T) {
			schema := doc.Components.Schemas[tt.schema]
			value := jsonValue(t, filepath.Join("../../../..", tt.file))
			if err := schema.Value.VisitJSON(value); err == nil {
				t.Fatal("the CUE rejects this fixture but the generated document accepts it")
			}
			tt.fix(t, mapOf(value))
			if err := schema.Value.VisitJSON(value); err != nil {
				t.Errorf("after supplying the rule's field the document still rejects it, so it failed for another reason:\n%v", err)
			}
		})
	}
}

// first returns the first element of a fixture list as a map.
func first(t *testing.T, list any, name string) map[string]any {
	t.Helper()
	items := sliceOf(list)
	if len(items) == 0 {
		t.Fatalf("the fixture has no %s; it no longer has the shape this test expects", name)
	}
	return mapOf(items[0])
}

// writeSourceModule writes src as the only file of a throwaway CUE module and
// returns its directory.
func writeSourceModule(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cue.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	module := "module: \"example.com/p@v0\"\nlanguage: version: \"v0.15.1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "cue.mod", "module.cue"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.cue"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// reloaded round-trips doc through the loader so every reference is resolved
// the same way a consumer of the written file would see it.
func reloaded(t *testing.T, doc *openapi3.T) *openapi3.T {
	t.Helper()
	data, err := doc.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	out, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
