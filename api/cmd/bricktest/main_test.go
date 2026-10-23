package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"trefoil/api/internal/conf"
)

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"core/os/tests.py", "core/os/config.yml", "code/go/config.yml", "code/node/tests.py"} {
		path := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"code:node", "core:os"}; !slices.Equal(refs, want) {
		t.Fatalf("discover = %v, want %v", refs, want)
	}
}

func TestTestContextAddsTestsAndRunner(t *testing.T) {
	b := &conf.Brick{Tools: []conf.Tool{{Name: "Read", Command: conf.Command{Language: "python", Code: "def Read(args): pass"}}}}
	bc, err := testContext("core:os", b, []byte("def test_x(): pass"))
	if err != nil {
		t.Fatal(err)
	}
	if string(bc.Files["tests/tests.py"]) != "def test_x(): pass" {
		t.Errorf("tests.py not in context")
	}
	if len(bc.Files["harness/brick_test.py"]) == 0 {
		t.Errorf("runner not in context")
	}
	if _, ok := bc.Files["tools/core_os/read.py"]; !ok {
		t.Errorf("brick tool not in context: %v", keys(bc.Files))
	}
	if !strings.Contains(bc.Dockerfile, "COPY tests/ /trefoil/tests/") {
		t.Errorf("Dockerfile doesn't copy tests:\n%s", bc.Dockerfile)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
