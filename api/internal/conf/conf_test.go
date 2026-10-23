package conf

import (
	"encoding/json"
	"testing"
)

func TestCommandCodeForms(t *testing.T) {
	cases := map[string]Command{
		`{"language":"python","code":"def Write(args): pass"}`:                                 {"python", "def Write(args): pass", ""},
		`{"language":"python","code":{"content":"def w(args): pass","entry":"w"}}`:             {"python", "def w(args): pass", "w"},
		`{"language":"python","code":{"content":"def w(args): pass"},"entry":"w"}`:             {"python", "def w(args): pass", "w"},
		`{"language":"python","code":{"content":"def w(args): pass","entry":"w"},"entry":"x"}`: {"python", "def w(args): pass", "w"},
	}
	for in, want := range cases {
		var got Command
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Errorf("%s: %v", in, err)
		} else if got != want {
			t.Errorf("%s: got %+v, want %+v", in, got, want)
		}
	}
	var c Command
	if err := json.Unmarshal([]byte(`{"language":"python","code":42}`), &c); err == nil {
		t.Error("want error for numeric code")
	}
}

func TestSkillSourceForms(t *testing.T) {
	for _, in := range []string{
		`{"name":"browser","source":"https://example.com/skill.zip"}`,
		`{"source":{"SKILL.md":"---\nname: notes\n---\n","ref/a.md":"A"}}`,
	} {
		var s Skill
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		// A packaged folder is written back as source, so stored configs
		// read the same way.
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Errorf("round trip: got %s, want %s", out, in)
		}
	}
	var s Skill
	if err := json.Unmarshal([]byte(`{"source":42}`), &s); err == nil {
		t.Error("want error for numeric source")
	}
}
