// SPDX-License-Identifier: Apache-2.0

package projection

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/literal"
	"cuelang.org/go/cue/token"
	"github.com/getkin/kin-openapi/openapi3"
)

// CUE CONVERTER WORKAROUND: helperRule is a cross-field conditional from a
// hidden #_ helper (such as #_AssessmentLogStrict) that prepare translated
// instead of dropping, because the upstream encoder cannot translate
// comprehensions. Two shapes are recognised, both of which only make a field
// required:
//
//	if subject == _|_ { field: T }                    // absent
//	if subject != "a" && subject != "b" { field: T }  // values
//
// T must be what the base definition already allows for field: #Def, or a
// non-empty list of #Def. applyHelperRulesTyped projects a rule only when that,
// and everything else the projection relies on, holds.
type helperRule struct {
	pos     string   // source position, for reports
	subject string   // field the condition tests
	absent  bool     // the condition is "subject is absent"
	values  []string // otherwise: "subject is none of values"
	field   string   // field the body requires
	def     string   // T is #def, or [#def, ...#def] when list is set
	list    bool
}

// translateHelperConditional reports whether c has one of the shapes
// helperRule represents, and the rule if so. Anything else is left for
// stripComprehensions to drop and report, as before.
func translateHelperConditional(c *ast.Comprehension) (helperRule, bool) {
	rule := helperRule{pos: c.Pos().String()}
	if len(c.Clauses) != 1 {
		return rule, false
	}
	clause, ok := c.Clauses[0].(*ast.IfClause)
	if !ok {
		return rule, false
	}
	if subject, ok := absentSubject(clause.Condition); ok {
		rule.subject, rule.absent = subject, true
	} else if subject, values, ok := excludedValues(clause.Condition); ok {
		rule.subject, rule.values = subject, values
	} else {
		return rule, false
	}
	// Hidden fields and definitions put no constraint on data.
	if !isDataField(rule.subject) {
		return rule, false
	}

	body, ok := c.Value.(*ast.StructLit)
	if !ok || len(body.Elts) != 1 {
		return rule, false
	}
	f, ok := body.Elts[0].(*ast.Field)
	// An optional or required (`!`) field in the body would mean something
	// other than "field must be present", so only a plain field qualifies.
	if !ok || f.Constraint != token.ILLEGAL || len(f.Attrs) != 0 {
		return rule, false
	}
	name, isIdent, err := ast.LabelName(f.Label)
	if err != nil || (isIdent && !isDataField(name)) {
		return rule, false
	}
	rule.field = name
	rule.def, rule.list, ok = bodyType(f.Value)
	return rule, ok
}

// isDataField reports whether an identifier names a regular field rather than
// a hidden field or a definition.
func isDataField(name string) bool {
	return !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "#")
}

// absentSubject matches `subject == _|_`.
func absentSubject(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BinaryExpr)
	if !ok || b.Op != token.EQL {
		return "", false
	}
	id, ok := b.X.(*ast.Ident)
	if _, bottom := b.Y.(*ast.BottomLit); !ok || !bottom {
		return "", false
	}
	return id.Name, true
}

// excludedValues matches `subject != "a"`, or a chain of them joined by &&
// that all test the same subject.
func excludedValues(e ast.Expr) (string, []string, bool) {
	b, ok := e.(*ast.BinaryExpr)
	if !ok {
		return "", nil, false
	}
	if b.Op == token.LAND {
		left, lv, lok := excludedValues(b.X)
		right, rv, rok := excludedValues(b.Y)
		if !lok || !rok || left != right {
			return "", nil, false
		}
		return left, append(lv, rv...), true
	}
	if b.Op != token.NEQ {
		return "", nil, false
	}
	id, isIdent := b.X.(*ast.Ident)
	lit, isLit := b.Y.(*ast.BasicLit)
	// token.STRING also covers bytes ('x'), which never equal a string in CUE.
	if !isIdent || !isLit || lit.Kind != token.STRING || !strings.HasPrefix(strings.TrimLeft(lit.Value, "#"), `"`) {
		return "", nil, false
	}
	value, err := literal.Unquote(lit.Value)
	if err != nil {
		return "", nil, false
	}
	return id.Name, []string{value}, true
}

// bodyType matches #Def or [#Def, ...#Def], the two forms a helper uses to
// require a field without narrowing it.
func bodyType(e ast.Expr) (def string, list, ok bool) {
	switch v := e.(type) {
	case *ast.Ident:
		if strings.HasPrefix(v.Name, "#") {
			return strings.TrimPrefix(v.Name, "#"), false, true
		}
	case *ast.ListLit:
		if len(v.Elts) != 2 {
			return "", false, false
		}
		first, ok := v.Elts[0].(*ast.Ident)
		if !ok || !strings.HasPrefix(first.Name, "#") {
			return "", false, false
		}
		rest, ok := v.Elts[1].(*ast.Ellipsis)
		if !ok {
			return "", false, false
		}
		if id, ok := rest.Type.(*ast.Ident); !ok || id.Name != first.Name {
			return "", false, false
		}
		return strings.TrimPrefix(first.Name, "#"), true, true
	}
	return "", false, false
}

// CUE CONVERTER WORKAROUND: applyHelperRules restores the hidden helpers'
// rules that stripComprehensions had to remove, and reports each rule it
// cannot project faithfully the same way Prep reports dropped conditionals.
func (c *Converter) applyHelperRules(doc *openapi3.T) error {
	skipped, err := applyHelperRulesTyped(doc, c.prep.HelperRules, c.value)
	if err != nil {
		return err
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "cue2openapi: %d hidden-helper rule(s) were not projected because OpenAPI would not match the CUE:\n", len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(os.Stderr, "  %s\n", s)
		}
	}
	return nil
}

// applyHelperRulesTyped attaches each translated helper rule wherever its
// helper is referenced, as an extra allOf branch beside the reference. It must
// run before collapseHelperSchemasTyped: once a reference to
// #_AssessmentLogStrict has been rewritten to #AssessmentLog, nothing records
// that the rule applied there. The rule is not added to the base schema,
// because the base definition does not carry it in CUE either.
//
// A rule that would not mean exactly what the CUE means is left out, and
// returned as a note, so generation never fails on account of a rule it could
// previously only omit.
func applyHelperRulesTyped(doc *openapi3.T, rules map[string][]helperRule, v cue.Value) ([]string, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	defaults, err := fieldDefaults(v)
	if err != nil {
		return nil, err
	}
	var skipped []string
	byRef := map[string][]helperRule{}
	for helper, hr := range rules {
		for _, r := range hr {
			if reason := unprojectableReason(doc.Components.Schemas, v, defaults, helper, r); reason != "" {
				skipped = append(skipped, fmt.Sprintf("%s: #%s: %s", r.pos, helper, reason))
				continue
			}
			byRef[refPrefix+helper] = append(byRef[refPrefix+helper], r)
		}
	}
	slices.Sort(skipped)
	return skipped, walkOpenAPISchemas(doc, func(s *openapi3.Schema) error {
		var extra openapi3.SchemaRefs
		for _, branch := range s.AllOf {
			extra = append(extra, helperRuleBranches(byRef[branch.Ref])...)
		}
		s.AllOf = append(s.AllOf, extra...)
		s.Items = withHelperRules(s.Items, byRef)
		for name, p := range s.Properties {
			s.Properties[name] = withHelperRules(p, byRef)
		}
		for i, branch := range s.AnyOf {
			s.AnyOf[i] = withHelperRules(branch, byRef)
		}
		for i, branch := range s.OneOf {
			s.OneOf[i] = withHelperRules(branch, byRef)
		}
		return nil
	})
}

// withHelperRules wraps a bare reference to a helper with rules as
// allOf [reference, rules...]; any other schema is returned unchanged.
func withHelperRules(ref *openapi3.SchemaRef, byRef map[string][]helperRule) *openapi3.SchemaRef {
	if ref == nil || len(byRef[ref.Ref]) == 0 {
		return ref
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{
		AllOf: append(openapi3.SchemaRefs{ref}, helperRuleBranches(byRef[ref.Ref])...),
	}}
}

// helperRuleBranches renders rules as fresh allOf branches, one per use site,
// so later passes never see one schema shared between two places.
func helperRuleBranches(rules []helperRule) openapi3.SchemaRefs {
	var out openapi3.SchemaRefs
	for _, r := range rules {
		unless := &openapi3.Schema{Required: []string{r.subject}}
		if !r.absent {
			values := make([]any, len(r.values))
			for i, v := range r.values {
				values[i] = v
			}
			unless = &openapi3.Schema{Properties: openapi3.Schemas{
				r.subject: {Value: &openapi3.Schema{Type: &openapi3.Types{openapi3.TypeString}, Enum: values}},
			}}
		}
		out = append(out, &openapi3.SchemaRef{Value: &openapi3.Schema{AnyOf: openapi3.SchemaRefs{
			{Value: unless},
			{Value: &openapi3.Schema{Required: []string{r.field}}},
		}}})
	}
	return out
}

// unprojectableReason returns why projecting r as "subject absent, or one of
// values; otherwise field required" would not say what the CUE says, or ""
// when it would.
func unprojectableReason(schemas openapi3.Schemas, v cue.Value, defaults map[string]any, helper string, r helperRule) string {
	h := schemas[helper]
	if h == nil || h.Value == nil {
		return "the encoder emitted no schema for it"
	}
	base, err := typedPassthroughBase(helper, h.Value, schemas)
	if err != nil {
		return err.Error()
	}
	props, required, err := typedResolved(base, schemas, map[string]bool{})
	if err != nil {
		return err.Error()
	}
	if props[r.subject] == nil {
		return fmt.Sprintf("#%s does not declare %s", base, r.subject)
	}
	// A default fills the field in CUE but is only advertised in OpenAPI, so
	// conditions on it, and requirements of it, would read differently.
	if _, ok := defaults[base+"."+r.subject]; ok {
		return fmt.Sprintf("%s has a default in #%s", r.subject, base)
	}
	// With an absent subject the CUE comparison never succeeds, while an enum
	// branch in OpenAPI passes vacuously. The two only agree when the base
	// already requires the subject.
	if !r.absent && !required[r.subject] {
		return fmt.Sprintf("%s is optional in #%s", r.subject, base)
	}
	field := props[r.field]
	if field == nil {
		return fmt.Sprintf("#%s does not declare %s", base, r.field)
	}
	if _, ok := defaults[base+"."+r.field]; ok {
		return fmt.Sprintf("%s has a default in #%s", r.field, base)
	}
	if !declaresType(field, r.def, r.list) {
		return fmt.Sprintf("#%s declares %s differently from the rule", base, r.field)
	}
	// `field: #Def` only means "field must be present" when #Def cannot be
	// made concrete without data, as a default or an all-optional struct can.
	def := v.LookupPath(cue.ParsePath("#" + r.def))
	if !def.Exists() {
		return fmt.Sprintf("#%s is not a definition in this package", r.def)
	}
	if def.Validate(cue.Concrete(true)) == nil {
		return fmt.Sprintf("#%s is satisfied without data, so %s is not effectively required", r.def, r.field)
	}
	return ""
}

// declaresType reports whether field is a reference to def, or with list set,
// a non-empty array of def. A nullable field also allows null, which the rule
// does not.
func declaresType(field *openapi3.SchemaRef, def string, list bool) bool {
	if !list {
		return refName(field) == def
	}
	return field.Value != nil && !field.Value.Nullable && field.Value.Type.Is("array") &&
		field.Value.MinItems >= 1 && refName(field.Value.Items) == def
}

// refName returns the schema name a reference points to, looking through the
// single-element allOf the encoder sometimes wraps a reference in.
func refName(ref *openapi3.SchemaRef) string {
	if ref == nil {
		return ""
	}
	if ref.Ref != "" {
		return strings.TrimPrefix(ref.Ref, refPrefix)
	}
	if ref.Value != nil && !ref.Value.Nullable && len(ref.Value.AllOf) == 1 {
		return refName(ref.Value.AllOf[0])
	}
	return ""
}
