package schema

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed conf.schema.json
var confSchema []byte

// Validator checks JSON documents against a compiled JSON Schema.
type Validator struct {
	schema *jsonschema.Schema
	brick  *jsonschema.Schema
}

// NewConf returns a validator for the /conf payload.
func NewConf() (*Validator, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(confSchema))
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("urn:trefoil:conf", doc); err != nil {
		return nil, err
	}
	s, err := c.Compile("urn:trefoil:conf")
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	b, err := c.Compile("urn:trefoil:conf#/$defs/brick")
	if err != nil {
		return nil, fmt.Errorf("compile brick schema: %w", err)
	}
	return &Validator{schema: s, brick: b}, nil
}

// Validate parses data as JSON and validates it. A malformed document and a
// schema violation both return an error.
func (v *Validator) Validate(data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return v.schema.Validate(inst)
}

// ValidateBrick validates a single brick, such as a shared brick loaded
// from disk.
func (v *Validator) ValidateBrick(data []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return v.brick.Validate(inst)
}
