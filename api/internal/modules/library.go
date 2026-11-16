// Package modules loads shared modules from a folder on the server.
//
// A shared module is referenced as "<namespace>:<name>" and lives at
// <dir>/<namespace>/<name>/config.yml. Its `!include file://...` values and bare
// `file://...` strings resolve relative to that folder, like the CLI packager;
// a ref to a folder becomes a map of relative path to file contents and a ref
// to a .zip a base64 data: URL (how skills are packaged).
//
// Every <module>/skills/<skill>/ folder with a SKILL.md is added to the
// module's skills, unless config.yml already refers to it.
package modules

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"trefoil/api/internal/conf"
)

var refPattern = regexp.MustCompile(`^([a-z0-9_-]+):([a-z0-9_-]+)$`)

// IsShared reports whether a module name refers to a shared module.
func IsShared(name string) bool {
	return strings.Contains(name, ":")
}

type Library struct {
	dir string
}

func NewLibrary(dir string) *Library {
	return &Library{dir: dir}
}

// Load reads and resolves a shared module, returning it as raw JSON (for
// schema validation) and as a typed module.
func (l *Library) Load(ref string) (json.RawMessage, *conf.Module, error) {
	m := refPattern.FindStringSubmatch(ref)
	if m == nil {
		return nil, nil, fmt.Errorf("invalid shared module %q: want <namespace>:<name>", ref)
	}
	moduleDir := filepath.Join(l.dir, m[1], m[2])
	data, err := os.ReadFile(filepath.Join(moduleDir, "config.yml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("shared module %q not found in %s", ref, l.dir)
	}
	if err != nil {
		return nil, nil, err
	}

	r := resolver{root: moduleDir}
	value, err := r.parse(data, moduleDir)
	if err != nil {
		return nil, nil, fmt.Errorf("shared module %q: %w", ref, err)
	}
	if value == nil {
		value = map[string]any{}
	}
	if m, ok := value.(map[string]any); ok {
		if err := r.addSkills(m, filepath.Join(moduleDir, "skills")); err != nil {
			return nil, nil, fmt.Errorf("shared module %q: %w", ref, err)
		}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	var b conf.Module
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil, fmt.Errorf("shared module %q: %w", ref, err)
	}
	return raw, &b, nil
}

// resolver turns YAML into JSON-compatible values, inlining file refs.
// Refs may not point outside the module's folder.
type resolver struct {
	root  string
	stack []string
	// dirs are the folders refs have resolved, so skill discovery skips them.
	dirs []string
}

// addSkills appends every <dir>/<skill>/SKILL.md folder not already referred
// to in the config to the module's skills.
func (r *resolver) addSkills(module map[string]any, dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	skills, _ := module["skills"].([]any)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if !e.IsDir() || slices.Contains(r.dirs, path) {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			continue
		}
		files, err := r.folder(path)
		if err != nil {
			return err
		}
		// Skills are named after their folder, as the Agent Skills spec has it.
		skills = append(skills, map[string]any{"name": e.Name(), "source": files})
	}
	if len(skills) > 0 {
		module["skills"] = skills
	}
	return nil
}

// folder reads every regular file under dir, skipping hidden files and
// __pycache__, as relative path (with forward slashes) to contents.
func (r *resolver) folder(dir string) (map[string]any, error) {
	r.dirs = append(r.dirs, dir)
	files := map[string]any{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if path != dir && (strings.HasPrefix(name, ".") || name == "__pycache__") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if !utf8.Valid(data) {
			return fmt.Errorf("%s: only text files are supported", filepath.Join(filepath.Base(dir), rel))
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return files, err
}

func (r *resolver) parse(data []byte, base string) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return r.value(doc.Content[0], base)
}

func (r *resolver) value(n *yaml.Node, base string) (any, error) {
	switch n.Kind {
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			v, err := r.value(n.Content[i+1], base)
			if err != nil {
				return nil, err
			}
			out[n.Content[i].Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := r.value(c, base)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.AliasNode:
		return r.value(n.Alias, base)
	case yaml.ScalarNode:
		if n.Tag == "!include" || n.Tag == "!Ref" || (n.Tag == "!!str" && strings.HasPrefix(n.Value, "file://")) {
			return r.ref(n.Value, base)
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	}
	return nil, fmt.Errorf("unsupported YAML node at line %d", n.Line)
}

func (r *resolver) ref(url, base string) (any, error) {
	rel, ok := strings.CutPrefix(url, "file://")
	if !ok {
		return nil, fmt.Errorf("unsupported ref %q: shared modules only support file:// refs", url)
	}
	path := filepath.Clean(filepath.Join(base, rel))
	if inside, err := filepath.Rel(r.root, path); err != nil || !filepath.IsLocal(inside) {
		return nil, fmt.Errorf("ref %q points outside the module folder", url)
	}
	for _, p := range r.stack {
		if p == path {
			return nil, fmt.Errorf("circular reference: %s", path)
		}
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return r.folder(path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ref %q: %w", url, err)
	}
	switch filepath.Ext(path) {
	case ".zip":
		return conf.ZipDataPrefix + base64.StdEncoding.EncodeToString(data), nil
	case ".yml", ".yaml", ".json":
		r.stack = append(r.stack, path)
		defer func() { r.stack = r.stack[:len(r.stack)-1] }()
		return r.parse(data, filepath.Dir(path))
	}
	return string(data), nil
}
