package conf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFieldsUnmarshal(t *testing.T) {
	var list, m Fields
	if err := json.Unmarshal([]byte(`[{"b":"string"},{"a":"integer?"}]`), &list); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"b":"string","a":"integer?"}`), &m); err != nil {
		t.Fatal(err)
	}
	if want := (Fields{{"b", "string"}, {"a", "integer?"}}); !reflect.DeepEqual(list, want) {
		t.Fatalf("list form = %v, want %v (order kept)", list, want)
	}
	if want := (Fields{{"a", "integer?"}, {"b", "string"}}); !reflect.DeepEqual(m, want) {
		t.Fatalf("map form = %v, want %v (sorted)", m, want)
	}
}

func TestFieldsCheck(t *testing.T) {
	if err := (Fields{{"a", "string[]?"}, {"b", "object"}}).Check(false); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		f       Fields
		outputs bool
	}{
		"unknown type":    {Fields{{"a", "text"}}, false},
		"duplicate":       {Fields{{"a", "string"}, {"a", "number"}}, false},
		"object output":   {Fields{{"a", "object"}}, true},
		"object[] output": {Fields{{"a", "object[]"}}, true},
	} {
		if err := tc.f.Check(tc.outputs); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestFieldsJSONSchema(t *testing.T) {
	got := Fields{{"cause", "string"}, {"changes", "string[]"}, {"n", "integer?"}}.JSONSchema()
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"cause":   map[string]any{"type": "string"},
			"changes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"n":       map[string]any{"type": "integer"},
		},
		"required":             []string{"cause", "changes"},
		"additionalProperties": false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema = %v", got)
	}
}

func TestInputSchema(t *testing.T) {
	var s InputSchema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{`+
		`"statement":{"type":"string","description":"What to say"},"times":{"type":"integer","minimum":1,"default":2},`+
		`"tags":{"type":"array","items":{"type":"string","enum":["a","b"]}},"extra":{}},"required":["statement"]}`), &s); err != nil {
		t.Fatal(err)
	}
	one := 1.0
	want := []Prop{
		{Field: Field{"statement", "string"}, Description: "What to say"},
		{Field: Field{"times", "integer?"}, Default: 2.0, Minimum: &one},
		{Field: Field{"tags", "string[]?"}, Enum: []any{"a", "b"}},
		{Field: Field{"extra", "any?"}},
	}
	if got := s.Props(); !reflect.DeepEqual(got, want) {
		t.Fatalf("props = %v, want %v", got, want)
	}
	if out, _ := json.Marshal(s); !strings.HasPrefix(string(out), `{"type":"object","properties":{"statement"`) {
		t.Fatalf("marshal should keep the config order: %s", out)
	}

	for _, in := range []string{`{"statement":"hi"}`, `{"statement":"hi","times":3,"tags":["a"]}`} {
		var m map[string]any
		json.Unmarshal([]byte(in), &m)
		if err := s.Validate(m); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
	for _, in := range []string{`{}`, `{"statement":1}`, `{"statement":"hi","times":1.5}`, `{"statement":"hi","tags":["a",2]}`} {
		var m map[string]any
		json.Unmarshal([]byte(in), &m)
		if err := s.Validate(m); err == nil {
			t.Errorf("%s: want error", in)
		}
	}
	var m map[string]any
	json.Unmarshal([]byte(`{}`), &m)
	if err := s.Validate(m); err == nil || err.Error() != "missing property 'statement'" {
		t.Errorf("error = %v", err)
	}
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"x":{"type":"text"}}}`), &s); err == nil {
		t.Error("bad schema: want error")
	}
}
