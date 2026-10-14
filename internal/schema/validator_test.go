package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// A deliberately tiny JSON Schema validator, enough to enforce the payload
// allowlist without a third-party dependency. Supported keywords: $ref (local
// "#/$defs/..." only), type (string or list), enum, pattern, minimum,
// minLength, properties, required, additionalProperties (boolean false),
// items, anyOf.

type jsonSchema = map[string]any

func validateJSON(root jsonSchema, s jsonSchema, v any, path string) []string {
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, path+": "+fmt.Sprintf(format, args...))
	}

	if ref, ok := s["$ref"].(string); ok {
		target, err := resolveRef(root, ref)
		if err != nil {
			fail("%v", err)
			return errs
		}
		errs = append(errs, validateJSON(root, target, v, path)...)
	}

	if t, ok := s["type"]; ok {
		var types []string
		switch tt := t.(type) {
		case string:
			types = []string{tt}
		case []any:
			for _, x := range tt {
				types = append(types, x.(string))
			}
		}
		matched := false
		for _, ty := range types {
			if jsonTypeMatches(ty, v) {
				matched = true
				break
			}
		}
		if !matched {
			fail("type mismatch: want %v, got %s", types, describe(v))
			return errs
		}
	}

	if anyOf, ok := s["anyOf"].([]any); ok {
		passed := false
		var best []string
		for _, sub := range anyOf {
			subErrs := validateJSON(root, sub.(jsonSchema), v, path)
			if len(subErrs) == 0 {
				passed = true
				break
			}
			// Keep the most specific failure (the branch that got past the
			// type check) so callers see e.g. the offending property name.
			if best == nil || len(subErrs) < len(best) || !strings.Contains(subErrs[0], "type mismatch") {
				best = subErrs
			}
		}
		if !passed {
			fail("value %s matched none of anyOf", describe(v))
			errs = append(errs, best...)
		}
	}

	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			fail("value %s not in enum %v", describe(v), enum)
		}
	}

	if str, ok := v.(string); ok {
		if p, ok := s["pattern"].(string); ok {
			if !regexp.MustCompile(p).MatchString(str) {
				fail("%q does not match pattern %s", str, p)
			}
		}
		if ml, ok := s["minLength"].(float64); ok && len(str) < int(ml) {
			fail("string shorter than minLength %d", int(ml))
		}
	}

	if num, ok := v.(float64); ok {
		if m, ok := s["minimum"].(float64); ok && num < m {
			fail("%v below minimum %v", num, m)
		}
	}

	if obj, ok := v.(map[string]any); ok {
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, present := obj[r.(string)]; !present {
					fail("missing required property %q", r)
				}
			}
		}
		for k, val := range obj {
			sub, declared := props[k]
			if !declared {
				if ap, ok := s["additionalProperties"].(bool); ok && !ap {
					fail("additional property %q not allowed", k)
				}
				continue
			}
			errs = append(errs, validateJSON(root, sub.(jsonSchema), val, path+"."+k)...)
		}
	}

	if arr, ok := v.([]any); ok {
		if items, ok := s["items"].(map[string]any); ok {
			for i, el := range arr {
				errs = append(errs, validateJSON(root, items, el, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}

	return errs
}

func resolveRef(root jsonSchema, ref string) (jsonSchema, error) {
	const prefix = "#/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	cur := any(root)
	for _, part := range strings.Split(strings.TrimPrefix(ref, prefix), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("bad $ref %q", ref)
		}
		cur, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("unresolved $ref %q", ref)
		}
	}
	out, ok := cur.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q is not a schema", ref)
	}
	return out, nil
}

func jsonTypeMatches(ty string, v any) bool {
	switch ty {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}

func describe(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 60 {
		s = s[:60] + "..."
	}
	return s
}
