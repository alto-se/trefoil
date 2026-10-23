package bricks

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trefoil/api/internal/conf"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "core", "os", "config.yml"), `
layers:
  - title: base
    script: echo hi
tools:
  - name: Write
    input_schema: { type: object, properties: { path: { type: string } } }
    command:
      language: python
      code: !Ref file://commands/write.py
`)
	writeFile(t, filepath.Join(dir, "core", "os", "commands", "write.py"), "def Write(args): pass\n")

	raw, b, err := NewLibrary(dir).Load("core:os")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Layers) != 1 || b.Layers[0].Script != "echo hi" {
		t.Fatalf("layers = %+v", b.Layers)
	}
	if len(b.Tools) != 1 || b.Tools[0].Command.Code != "def Write(args): pass\n" || b.Tools[0].InputSchema["type"] != "object" {
		t.Fatalf("tools = %+v", b.Tools)
	}
	if !strings.Contains(string(raw), `"script":"echo hi"`) {
		t.Fatalf("raw = %s", raw)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "core", "escape", "config.yml"), "source: !Ref file://../../secret.txt\n")
	writeFile(t, filepath.Join(dir, "secret.txt"), "nope")
	writeFile(t, filepath.Join(dir, "core", "remote", "config.yml"), "source: !Ref https://example.com/x\n")

	lib := NewLibrary(dir)
	for ref, want := range map[string]string{
		"core:missing":  "not found",
		"core:escape":   "outside the brick folder",
		"core:remote":   "only support file://",
		"../etc:passwd": "invalid shared brick",
		"plain":         "invalid shared brick",
	} {
		if _, _, err := lib.Load(ref); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) err = %v, want %q", ref, err, want)
		}
	}
}

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	brick := filepath.Join(dir, "docs", "notes")
	writeFile(t, filepath.Join(brick, "config.yml"), `
skills:
  - source: https://example.com/remote.zip
  - name: listed
    source: file://skills/listed
`)
	writeFile(t, filepath.Join(brick, "skills", "listed", "SKILL.md"), "---\nname: listed\n---\n")
	writeFile(t, filepath.Join(brick, "skills", "found", "SKILL.md"), "---\nname: found\n---\nSee ref/a.md\n")
	writeFile(t, filepath.Join(brick, "skills", "found", "ref", "a.md"), "A\n")
	writeFile(t, filepath.Join(brick, "skills", "found", ".DS_Store"), "x")
	writeFile(t, filepath.Join(brick, "skills", "found", "__pycache__", "x.pyc"), "x")
	writeFile(t, filepath.Join(brick, "skills", "not-a-skill", "README.md"), "no SKILL.md here")

	_, b, err := NewLibrary(dir).Load("docs:notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Skills) != 3 {
		t.Fatalf("skills = %+v", b.Skills)
	}
	if b.Skills[0].Source != "https://example.com/remote.zip" || b.Skills[1].Name != "listed" || b.Skills[1].Files["SKILL.md"] == "" {
		t.Fatalf("listed skills = %+v", b.Skills[:2])
	}
	found := b.Skills[2].Files
	if b.Skills[2].Name != "found" || len(found) != 2 || found["ref/a.md"] != "A\n" {
		t.Fatalf("discovered skill files = %v", found)
	}
}

func TestLoadSkillZip(t *testing.T) {
	dir := t.TempDir()
	brick := filepath.Join(dir, "docs", "zipped")
	writeFile(t, filepath.Join(brick, "config.yml"), "skills:\n  - source: !Ref file://skills/notes.zip\n")
	writeFile(t, filepath.Join(brick, "skills", "notes.zip"), "PK\x03\x04\xff")
	_, b, err := NewLibrary(dir).Load("docs:zipped")
	if err != nil {
		t.Fatal(err)
	}
	want := conf.ZipDataPrefix + base64.StdEncoding.EncodeToString([]byte("PK\x03\x04\xff"))
	if len(b.Skills) != 1 || b.Skills[0].Source != want {
		t.Fatalf("skills = %+v", b.Skills)
	}
}

func TestLoadSkillsBinary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs", "bin", "config.yml"), "")
	writeFile(t, filepath.Join(dir, "docs", "bin", "skills", "s", "SKILL.md"), "---\nname: s\n---\n")
	writeFile(t, filepath.Join(dir, "docs", "bin", "skills", "s", "logo.png"), "\x89PNG\xff\xfe")
	if _, _, err := NewLibrary(dir).Load("docs:bin"); err == nil || !strings.Contains(err.Error(), "only text files") {
		t.Fatalf("err = %v", err)
	}
}
