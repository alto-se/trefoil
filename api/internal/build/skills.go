package build

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"trefoil/api/internal/conf"
)

// skillSpec is an entry in agent.json "skills". The harness finds installed
// skills in /trefoil/skills itself; this records where each came from.
type skillSpec struct {
	Brick string `json:"brick"`
	// Name is empty for a URL skill named by its own SKILL.md.
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"`
}

var skillNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// packagedSkill reports whether s is packaged into the config, as a folder or
// a zip, rather than fetched from a URL at build time.
func packagedSkill(s conf.Skill) bool {
	return s.Files != nil || strings.HasPrefix(s.Source, conf.ZipDataPrefix)
}

// skillFiles checks a packaged skill folder or zip and returns its name and
// its files as build context paths under skills/<name>/.
func skillFiles(s conf.Skill) (string, map[string][]byte, error) {
	src := map[string][]byte{}
	for rel, data := range s.Files {
		src[rel] = []byte(data)
	}
	if s.Files == nil {
		var err error
		if src, err = unzipSkill(strings.TrimPrefix(s.Source, conf.ZipDataPrefix)); err != nil {
			return "", nil, fmt.Errorf("skill %q: %w", s.Name, err)
		}
	}
	skillMD, ok := src["SKILL.md"]
	if !ok {
		return "", nil, fmt.Errorf("skill %q has no SKILL.md", s.Name)
	}
	name := s.Name
	if name == "" {
		meta, err := frontMatter(string(skillMD))
		if err != nil {
			return "", nil, fmt.Errorf("SKILL.md front matter: %w", err)
		}
		name, _ = meta["name"].(string)
	}
	if !skillNamePattern.MatchString(name) {
		return "", nil, fmt.Errorf("skill name %q: want letters, digits, '_', '.' or '-' (set name in SKILL.md)", name)
	}
	files := map[string][]byte{}
	for rel, data := range src {
		if clean := path.Clean(rel); clean != rel || !isLocal(rel) {
			return "", nil, fmt.Errorf("skill %q: file path %q must be relative and inside the skill", name, rel)
		}
		files[path.Join("skills", name, rel)] = data
	}
	return name, files, nil
}

// unzipSkill decodes a base64 zip and returns the files of its skill folder,
// relative to it: the shallowest folder with a SKILL.md, as for URL zips, so
// a zip of the folder itself or of its parent both work.
func unzipSkill(b64 string) (map[string][]byte, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decoding zip: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("reading zip: %w", err)
	}
	var roots []string
	for _, f := range zr.File {
		if path.Base(f.Name) == "SKILL.md" {
			roots = append(roots, path.Dir(f.Name))
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no SKILL.md in zip")
	}
	depth := func(dir string) int {
		if dir == "." {
			return 0
		}
		return strings.Count(dir, "/") + 1
	}
	sort.SliceStable(roots, func(i, j int) bool { return depth(roots[i]) < depth(roots[j]) })
	prefix := ""
	if roots[0] != "." {
		prefix = roots[0] + "/"
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || f.FileInfo().IsDir() || hiddenPath(rel) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		files[rel] = data
	}
	return files, nil
}

// hiddenPath reports whether rel is in or is a dot folder or __pycache__,
// which folder skills leave out too (e.g. __MACOSX/ in zips made on a Mac).
func hiddenPath(rel string) bool {
	for _, p := range strings.Split(rel, "/") {
		if strings.HasPrefix(p, ".") || p == "__pycache__" || p == "__MACOSX" {
			return true
		}
	}
	return false
}

// SkillDescription is the description in the SKILL.md front matter of a
// skill packaged into bc, or "" for a skill fetched at build time.
func SkillDescription(bc *Context, name string) string {
	meta, err := frontMatter(string(bc.Files[path.Join("skills", name, "SKILL.md")]))
	if err != nil {
		return ""
	}
	d, _ := meta["description"].(string)
	return d
}

func isLocal(rel string) bool {
	return rel != "" && rel != "." && !path.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, "../")
}

// frontMatter parses the YAML between the leading "---" lines of a SKILL.md.
func frontMatter(text string) (map[string]any, error) {
	rest, ok := strings.CutPrefix(strings.ReplaceAll(strings.TrimPrefix(text, "\uFEFF"), "\r\n", "\n"), "---\n")
	if !ok {
		return nil, fmt.Errorf("missing (want a leading --- block with name and description)")
	}
	block, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil, fmt.Errorf("unterminated --- block")
	}
	meta := map[string]any{}
	if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
		return nil, err
	}
	return meta, nil
}
