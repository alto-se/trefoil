package conf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// InputSchema is how an agent is called: a JSON Schema object, written the
// same way as a tool's input_schema.
//
//	input_schema:
//	  type: object
//	  properties:
//	    statement: { type: string, description: "What to echo" }
//	    times: { type: integer }
//	  required: [statement]
//
// The raw JSON is kept so properties stay in config order.
type InputSchema struct {
	raw    json.RawMessage
	schema *jsonschema.Schema
}

func (s *InputSchema) UnmarshalJSON(data []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("urn:trefoil:input", doc); err != nil {
		return err
	}
	compiled, err := c.Compile("urn:trefoil:input")
	if err != nil {
		return fmt.Errorf("input_schema: %w", err)
	}
	s.raw = slices.Clone(data)
	s.schema = compiled
	return nil
}

func (s InputSchema) MarshalJSON() ([]byte, error) {
	if s.raw == nil {
		return []byte("null"), nil
	}
	return s.raw, nil
}

// Map returns the schema as a plain JSON object.
func (s *InputSchema) Map() map[string]any {
	var m map[string]any
	json.Unmarshal(s.raw, &m)
	return m
}

// Prop is one top-level property of an input schema. Its Field type is the
// short form used in forms and listings, e.g. "integer[]?" for an optional
// list of integers, or "any" when the schema doesn't name one type. Enum,
// Default and the bounds come from the property, Enum from its items for a
// list.
type Prop struct {
	Field
	Description string
	Enum        []any
	Default     any
	Minimum     *float64
	Maximum     *float64
}

// Props returns the schema's top-level properties in config order.
func (s *InputSchema) Props() []Prop {
	if s == nil {
		return nil
	}
	var doc struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if json.Unmarshal(s.raw, &doc) != nil || len(doc.Properties) == 0 {
		return nil
	}
	var out []Prop
	dec := json.NewDecoder(bytes.NewReader(doc.Properties))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return out
		}
		name, _ := t.(string)
		var p struct {
			Type        any      `json:"type"`
			Description string   `json:"description"`
			Enum        []any    `json:"enum"`
			Default     any      `json:"default"`
			Minimum     *float64 `json:"minimum"`
			Maximum     *float64 `json:"maximum"`
			Items       struct {
				Type any   `json:"type"`
				Enum []any `json:"enum"`
			} `json:"items"`
		}
		if err := dec.Decode(&p); err != nil {
			return out
		}
		typ, _ := p.Type.(string)
		enum := p.Enum
		if typ == "array" {
			enum = p.Items.Enum
			if item, ok := p.Items.Type.(string); ok {
				typ = item + "[]"
			}
		}
		if typ == "" {
			typ = "any"
		}
		if !slices.Contains(doc.Required, name) {
			typ += "?"
		}
		out = append(out, Prop{Field: Field{Name: name, Type: typ}, Description: p.Description,
			Enum: enum, Default: p.Default, Minimum: p.Minimum, Maximum: p.Maximum})
	}
	return out
}

// ValidatePartial checks values against the schema without its required
// list, for params, which are set one at a time.
func (s *InputSchema) ValidatePartial(values map[string]any) error {
	var m map[string]any
	if err := json.Unmarshal(s.raw, &m); err != nil {
		return err
	}
	delete(m, "required")
	raw, _ := json.Marshal(m)
	var partial InputSchema
	if err := partial.UnmarshalJSON(raw); err != nil {
		return err
	}
	return partial.Validate(values)
}

// Validate checks a run's input against the schema.
func (s *InputSchema) Validate(input map[string]any) error {
	err := s.schema.Validate(input)
	var ve *jsonschema.ValidationError
	if errors.As(err, &ve) {
		// Drop the "jsonschema validation failed" header and list markers:
		// "- at '/times': got string, want integer" -> "at '/times': ...".
		var msgs []string
		for _, line := range strings.Split(ve.Error(), "\n")[1:] {
			line = strings.TrimPrefix(strings.TrimSpace(line), "- ")
			if line = strings.TrimPrefix(line, "at '': "); line != "" {
				msgs = append(msgs, line)
			}
		}
		return errors.New(strings.Join(msgs, "; "))
	}
	return err
}
