package conf

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Field is one named, typed value in an agent's outputs. Type is
// a base type, optionally suffixed "[]" for a list and "?" for optional:
// "string", "integer[]", "boolean?".
type Field struct {
	Name string
	Type string
}

// Fields is what an agent returns (outputs). In config it is either a list
// of single-entry maps, which keeps the order, or one map:
//
//	outputs:
//	  - echo: string
//	  - times: integer?
type Fields []Field

var baseTypes = []string{"string", "number", "integer", "boolean", "object", "array"}

func (f *Fields) UnmarshalJSON(data []byte) error {
	var list []map[string]string
	if err := json.Unmarshal(data, &list); err == nil {
		for _, m := range list {
			*f = append(*f, sortedFields(m)...)
		}
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf(`want a list of {name: type} or a {name: type} map`)
	}
	*f = sortedFields(m)
	return nil
}

func (f Fields) MarshalJSON() ([]byte, error) {
	list := make([]map[string]string, len(f))
	for i, field := range f {
		list[i] = map[string]string{field.Name: field.Type}
	}
	return json.Marshal(list)
}

func sortedFields(m map[string]string) Fields {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	var out Fields
	for _, name := range names {
		out = append(out, Field{Name: name, Type: m[name]})
	}
	return out
}

// Parts splits the field's type: "integer[]?" is ("integer", list, optional).
func (f Field) Parts() (base string, list, optional bool) {
	return parseType(f.Type)
}

// parseType splits "integer[]?" into ("integer", list, optional).
func parseType(t string) (base string, list, optional bool) {
	t = strings.TrimSpace(t)
	t, optional = strings.CutSuffix(t, "?")
	t, list = strings.CutSuffix(t, "[]")
	return t, list, optional
}

// Check reports unknown types and duplicate names. Outputs can't use a bare
// object: structured outputs only accept objects with known properties.
func (f Fields) Check(outputs bool) error {
	seen := map[string]bool{}
	for _, field := range f {
		if field.Name == "" {
			return fmt.Errorf("empty field name")
		}
		if seen[field.Name] {
			return fmt.Errorf("duplicate field %q", field.Name)
		}
		seen[field.Name] = true
		base, _, _ := parseType(field.Type)
		if !slices.Contains(baseTypes, base) {
			return fmt.Errorf("field %q: unknown type %q (want %s, optionally with [] and/or ?)",
				field.Name, field.Type, strings.Join(baseTypes, ", "))
		}
		if outputs && base == "object" {
			return fmt.Errorf("field %q: outputs can't be a free-form object", field.Name)
		}
	}
	return nil
}

// JSONSchema returns the object schema for the fields, in the strict form
// structured outputs require (additionalProperties false).
func (f Fields) JSONSchema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, field := range f {
		base, list, optional := parseType(field.Type)
		var s map[string]any
		if base == "array" {
			s = map[string]any{"type": "array", "items": map[string]any{}}
		} else {
			s = map[string]any{"type": base}
		}
		if list {
			s = map[string]any{"type": "array", "items": s}
		}
		props[field.Name] = s
		if !optional {
			required = append(required, field.Name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}
