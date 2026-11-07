// Package oas validates JSON values against schemas in spec/openapi.yaml.
// It implements the subset of JSON Schema the contract uses: $ref, type
// (including "null" in type lists), required, properties,
// additionalProperties (false or schema), items, enum, const, pattern,
// minimum, maximum, minLength, minItems, maxItems, allOf and oneOf.
package oas

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Spec is a loaded OpenAPI document.
type Spec struct {
	doc map[string]any
}

// Load reads an OpenAPI YAML file.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &Spec{doc: doc}, nil
}

// LoadRepo loads spec/openapi.yaml from the repo root.
func LoadRepo(root string) (*Spec, error) {
	return Load(filepath.Join(root, "spec", "openapi.yaml"))
}

func (s *Spec) resolve(ref string) (map[string]any, error) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	var cur any = s.doc
	for _, part := range strings.Split(ref[2:], "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("$ref %q: %q is not an object", ref, part)
		}
		if cur, ok = m[part]; !ok {
			return nil, fmt.Errorf("$ref %q: %q not found", ref, part)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q does not point to a schema", ref)
	}
	return m, nil
}

// Refs returns every $ref in the document that does not resolve.
func (s *Spec) Refs() []string {
	var bad []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if r, ok := x["$ref"].(string); ok {
				if _, err := s.resolve(r); err != nil {
					bad = append(bad, err.Error())
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(s.doc)
	sort.Strings(bad)
	return bad
}

// ValidateJSON decodes data and validates it against components/schemas/<name>.
func (s *Spec) ValidateJSON(name string, data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("not JSON: %v (%q)", err, truncate(string(data)))
	}
	schema, err := s.resolve("#/components/schemas/" + name)
	if err != nil {
		return err
	}
	var errs []string
	s.validate(schema, v, "$", &errs)
	if len(errs) > 0 {
		return fmt.Errorf("does not match schema %s:\n  %s", name, strings.Join(errs, "\n  "))
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64:
		if x == math.Trunc(x) {
			return "integer"
		}
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func typeMatches(want string, v any) bool {
	got := typeOf(v)
	return got == want || (want == "number" && got == "integer")
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func (s *Spec) validate(schema map[string]any, v any, path string, errs *[]string) {
	fail := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	if ref, ok := schema["$ref"].(string); ok {
		target, err := s.resolve(ref)
		if err != nil {
			fail("%v", err)
			return
		}
		s.validate(target, v, path, errs)
		return
	}
	if all, ok := schema["allOf"].([]any); ok {
		for _, sub := range all {
			if m, ok := sub.(map[string]any); ok {
				s.validateNoExtras(m, v, path, errs)
			}
		}
	}
	if one, ok := schema["oneOf"].([]any); ok {
		matches := 0
		for _, sub := range one {
			if m, ok := sub.(map[string]any); ok {
				var sube []string
				s.validate(m, v, path, &sube)
				if len(sube) == 0 {
					matches++
				}
			}
		}
		if matches != 1 {
			fail("matches %d of the oneOf alternatives, want exactly 1", matches)
		}
	}
	switch t := schema["type"].(type) {
	case string:
		if !typeMatches(t, v) {
			fail("type %s, want %s (value %v)", typeOf(v), t, v)
			return
		}
	case []any:
		ok := false
		for _, x := range t {
			if ts, _ := x.(string); typeMatches(ts, v) {
				ok = true
			}
		}
		if !ok {
			fail("type %s, want one of %v", typeOf(v), t)
			return
		}
	}
	if c, ok := schema["const"]; ok && fmt.Sprint(c) != fmt.Sprint(v) {
		fail("value %v, want const %v", v, c)
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) || (e == nil && v == nil) {
				found = true
			}
		}
		if !found {
			fail("value %v not in enum %v", v, enum)
		}
	}
	if str, ok := v.(string); ok {
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			fail("%q does not match pattern %s", str, p)
		}
		if n, ok := num(schema["minLength"]); ok && float64(len([]rune(str))) < n {
			fail("shorter than minLength %v", n)
		}
	}
	if f, ok := v.(float64); ok {
		if n, ok := num(schema["minimum"]); ok && f < n {
			fail("%v is below minimum %v", f, n)
		}
		if n, ok := num(schema["maximum"]); ok && f > n {
			fail("%v is above maximum %v", f, n)
		}
	}
	if arr, ok := v.([]any); ok {
		if n, ok := num(schema["minItems"]); ok && float64(len(arr)) < n {
			fail("fewer than %v items", n)
		}
		if n, ok := num(schema["maxItems"]); ok && float64(len(arr)) > n {
			fail("more than %v items", n)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, x := range arr {
				s.validate(items, x, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if _, ok := obj[r.(string)]; !ok {
					fail("missing required property %q", r)
				}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				s.validate(ps, obj[k], path+"."+k, errs)
				continue
			}
			switch ap := schema["additionalProperties"].(type) {
			case bool:
				if !ap {
					fail("unexpected property %q", k)
				}
			case map[string]any:
				s.validate(ap, obj[k], path+"."+k, errs)
			}
		}
	}
}

// validateNoExtras validates an allOf branch without applying its
// additionalProperties (allOf branches see only their own properties).
func (s *Spec) validateNoExtras(schema map[string]any, v any, path string, errs *[]string) {
	cp := map[string]any{}
	for k, x := range schema {
		if k != "additionalProperties" {
			cp[k] = x
		}
	}
	s.validate(cp, v, path, errs)
}
