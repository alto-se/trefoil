package build

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"trefoil/api/internal/conf"
)

func TestCompose(t *testing.T) {
	c := &conf.Conf{
		Modules: map[string]conf.Module{
			"os": {
				Layers: []conf.Layer{{Title: "base", Script: "apt-get update"}},
				Tools:  []conf.Tool{{Name: "Write", Command: conf.Command{Language: "python", Code: "def Write(args): pass"}}},
				Skills: []conf.Skill{{Files: map[string]string{
					"SKILL.md":           "---\nname: notes\ndescription: Writes notes\n---\nUse template.md.\n",
					"assets/template.md": "# Notes\n",
				}}},
			},
			"chromium": {
				Layers: []conf.Layer{{Title: "install\nchromium", Script: "apt-get install -y chromium\nchromium --version"}},
				Skills: []conf.Skill{{Name: "browser", Source: "https://example.com/skill.zip"}},
			},
			"search": {
				ServerTools: []conf.ServerTool{{"name": "web_search", "max_uses": 3}},
			},
		},
		Defaults: conf.Defaults{Modules: []string{"os"}, Model: "m"},
	}
	agent := &conf.Agent{Name: "fixer", Modules: []string{"chromium", "os", "search"}}

	bc, err := Compose(c, agent)
	if err != nil {
		t.Fatal(err)
	}

	want := `# syntax=docker/dockerfile:1
FROM python:3.12-slim
ARG DEBIAN_FRONTEND=noninteractive

# os: base
RUN apt-get update

# chromium: install chromium
RUN <<'TREFOIL_EOF'
set -e
apt-get install -y chromium
chromium --version
TREFOIL_EOF

# skills from URLs
COPY harness/skills.py /trefoil/harness/skills.py
RUN ["python","/trefoil/harness/skills.py","fetch","https://example.com/skill.zip","browser"]

# trefoil harness
RUN pip install --no-cache-dir 'anthropic>=2,<3'
COPY harness/ /trefoil/harness/
COPY tools/ /trefoil/tools/
COPY skills/ /trefoil/skills/
COPY agent.json /trefoil/agent.json
ENV TREFOIL_AGENT=/trefoil/agent.json
WORKDIR /workspace
CMD ["python", "/trefoil/harness/main.py"]
`
	if bc.Dockerfile != want {
		t.Fatalf("Dockerfile:\n%s\nwant:\n%s", bc.Dockerfile, want)
	}

	if got := string(bc.Files["tools/os/write.py"]); got != "def Write(args): pass" {
		t.Fatalf("tool file = %q", got)
	}
	if got := string(bc.Files["skills/notes/assets/template.md"]); got != "# Notes\n" {
		t.Fatalf("skill file = %q", got)
	}
	if _, ok := bc.Files["skills/notes/SKILL.md"]; !ok {
		t.Fatal("SKILL.md not in build context")
	}

	var spec agentSpec
	if err := json.Unmarshal(bc.Files["agent.json"], &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.ServerTools) != 1 || spec.ServerTools[0].Name() != "web_search" {
		t.Fatalf("server tools = %+v", spec.ServerTools)
	}
	if strings.Join(spec.Modules, ",") != "os,chromium,search" || len(spec.Tools) != 1 || spec.Tools[0].Path != "/trefoil/tools/os/write.py" ||
		len(spec.Skills) != 2 || spec.Skills[0] != (skillSpec{Module: "os", Name: "notes"}) {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestComposeBedrock(t *testing.T) {
	c := &conf.Conf{
		Modules:  map[string]conf.Module{},
		Defaults: conf.Defaults{Model: "m", Provider: "bedrock", AWSRegion: "eu-west-1"},
	}
	bc, err := Compose(c, &conf.Agent{Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bc.Dockerfile, "'anthropic[bedrock]>=2,<3'") {
		t.Fatalf("Dockerfile missing bedrock extra:\n%s", bc.Dockerfile)
	}
	var spec agentSpec
	if err := json.Unmarshal(bc.Files["agent.json"], &spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bc.Files["agent.json"]), `"modules": []`) {
		t.Fatalf("agent without modules should encode modules as []:\n%s", bc.Files["agent.json"])
	}
	if spec.Provider != "bedrock" || spec.AWSRegion != "eu-west-1" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestComposeUnknownModule(t *testing.T) {
	c := &conf.Conf{Modules: map[string]conf.Module{}}
	if _, err := Compose(c, &conf.Agent{Name: "a", Modules: []string{"nope"}}); err == nil {
		t.Fatal("want error for unknown module")
	}
}

func TestImageTag(t *testing.T) {
	if got := ImageTag("Error Fixer!"); got != "trefoil-agent-error-fixer:latest" {
		t.Fatalf("ImageTag = %q", got)
	}
}

func TestComposeMCP(t *testing.T) {
	c := &conf.Conf{
		Modules: map[string]conf.Module{"gh": {
			Secrets: map[string]conf.Secret{"github_token": {Required: true}},
			MCPs:    []conf.MCP{{Name: "github", URL: "https://example.com/mcp", Credentials: map[string]string{"Authorization": "Bearer ${GITHUB_TOKEN}"}}},
		}},
		Agents: []conf.Agent{{Name: "a", Modules: []string{"gh"}}},
	}
	bc, err := Compose(c, &c.Agents[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bc.Dockerfile, "pip install --no-cache-dir 'mcp>=") {
		t.Fatalf("Dockerfile has no mcp install:\n%s", bc.Dockerfile)
	}
	if !strings.Contains(string(bc.Files["agent.json"]), `"Authorization": "Bearer ${GITHUB_TOKEN}"`) {
		t.Fatalf("agent.json = %s", bc.Files["agent.json"])
	}
	if s := AgentSecrets(c, &c.Agents[0]); !s["github_token"].Required {
		t.Fatalf("AgentSecrets = %v", s)
	}
}

func TestComposeSkillErrors(t *testing.T) {
	for want, skill := range map[string]conf.Skill{
		"no SKILL.md":           {Files: map[string]string{"README.md": ""}},
		"front matter: missing": {Files: map[string]string{"SKILL.md": "# no front matter"}},
		"skill name \"\"":       {Files: map[string]string{"SKILL.md": "---\ndescription: x\n---\n"}},
		"inside the skill":      {Name: "x", Files: map[string]string{"SKILL.md": "", "../escape": ""}},
	} {
		c := &conf.Conf{Modules: map[string]conf.Module{"b": {Skills: []conf.Skill{skill}}}}
		if _, err := Compose(c, &conf.Agent{Name: "a", Modules: []string{"b"}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}

	skill := conf.Skill{Files: map[string]string{"SKILL.md": "---\r\nname: dup\r\n---\r\n"}}
	c := &conf.Conf{Modules: map[string]conf.Module{"a": {Skills: []conf.Skill{skill}}, "b": {Skills: []conf.Skill{skill}}}}
	if _, err := Compose(c, &conf.Agent{Name: "x", Modules: []string{"a", "b"}}); err == nil || !strings.Contains(err.Error(), "in both module") {
		t.Errorf("duplicate skill err = %v", err)
	}
}

// zipSource zips files the way `!include file://skill.zip` packages them.
func zipSource(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return conf.ZipDataPrefix + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestComposeZipSkill(t *testing.T) {
	src := zipSource(t, map[string]string{
		"proofread/SKILL.md":                "---\nname: proofread\ndescription: Proofreads\n---\n",
		"proofread/checklist.md":            "- typos\n",
		"proofread/examples/SKILL.md":       "---\nname: nested\n---\n",
		"__MACOSX/proofread/._checklist.md": "junk",
	})
	c := &conf.Conf{Modules: map[string]conf.Module{"b": {Skills: []conf.Skill{{Source: src}}}}}
	bc, err := Compose(c, &conf.Agent{Name: "a", Modules: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for p := range bc.Files {
		if strings.HasPrefix(p, "skills/") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	want := []string{"skills/proofread/SKILL.md", "skills/proofread/checklist.md", "skills/proofread/examples/SKILL.md"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("skill files = %v, want %v", paths, want)
	}
	if strings.Contains(bc.Dockerfile, "skills.py") || !strings.Contains(bc.Dockerfile, "COPY skills/") {
		t.Fatalf("zip skill should be copied, not fetched:\n%s", bc.Dockerfile)
	}
	if strings.Contains(string(bc.Files["agent.json"]), "base64") {
		t.Fatalf("agent.json should not carry the zip:\n%s", bc.Files["agent.json"])
	}

	for want, source := range map[string]string{
		"decoding zip":    conf.ZipDataPrefix + "not base64!",
		"reading zip":     conf.ZipDataPrefix + base64.StdEncoding.EncodeToString([]byte("not a zip")),
		"no SKILL.md":     zipSource(t, map[string]string{"README.md": ""}),
		"skill name \"\"": zipSource(t, map[string]string{"SKILL.md": "---\ndescription: x\n---\n"}),
	} {
		c := &conf.Conf{Modules: map[string]conf.Module{"b": {Skills: []conf.Skill{{Source: source}}}}}
		if _, err := Compose(c, &conf.Agent{Name: "a", Modules: []string{"b"}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestComposeMCPSigV4(t *testing.T) {
	c := &conf.Conf{
		Modules: map[string]conf.Module{"aws": {
			MCPs: []conf.MCP{{Name: "aws", URL: "https://aws-mcp.us-east-1.api.aws/mcp", SigV4: &conf.SigV4{Service: "aws-mcp", Region: "us-east-1"}}},
		}},
		Agents: []conf.Agent{{Name: "a", Modules: []string{"aws"}}},
	}
	bc, err := Compose(c, &c.Agents[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bc.Dockerfile, "pip install --no-cache-dir botocore") {
		t.Fatalf("Dockerfile has no botocore install:\n%s", bc.Dockerfile)
	}
	if !strings.Contains(string(bc.Files["agent.json"]), `"service": "aws-mcp"`) {
		t.Fatalf("agent.json = %s", bc.Files["agent.json"])
	}
}
