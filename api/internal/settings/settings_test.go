package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env", "# creds\nexport TREFOIL_TEST_KEY=\"from-file\"\nTREFOIL_TEST_SET=from-file\n")
	path := write(t, dir, "backend.yml", `
env_file: .env
aws:
  region: eu-west-1
llm_provider:
  - bedrock
storage:
  default: local
server_port: 9090
`)
	t.Setenv("DATA_DIR", "/override")
	t.Setenv("TREFOIL_TEST_SET", "from-env")
	t.Setenv("TREFOIL_TEST_KEY", "")
	os.Unsetenv("TREFOIL_TEST_KEY")

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != ":9090" || s.Storage.Local.Path != "/override" || s.ModulesDir != filepath.Join(dir, "modules") {
		t.Fatalf("settings = %+v", s)
	}
	if s.AgentEnv()["AWS_REGION"] != "eu-west-1" || s.LLMProvider[0] != "bedrock" {
		t.Fatalf("settings = %+v", s)
	}
	if os.Getenv("TREFOIL_TEST_KEY") != "from-file" || os.Getenv("TREFOIL_TEST_SET") != "from-env" {
		t.Fatalf("env: key=%q set=%q", os.Getenv("TREFOIL_TEST_KEY"), os.Getenv("TREFOIL_TEST_SET"))
	}
}

func TestLoadNoFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != ":8080" || s.Storage.Local.Path != filepath.Join(home, ".trefoil", "data") {
		t.Fatalf("settings = %+v", s)
	}
}

func TestLoadExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MODULES_DIR", "~/modules")
	s, err := Load(write(t, t.TempDir(), "backend.yml", "storage:\n  local:\n    path: ~/state\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Storage.Local.Path != filepath.Join(home, "state") || s.ModulesDir != filepath.Join(home, "modules") {
		t.Fatalf("settings = %+v", s)
	}
}

func TestLoadProviderEnv(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "anthropic, bedrock")
	s, err := Load(write(t, t.TempDir(), "backend.yml", "llm_provider: [bedrock]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.LLMProvider) != 2 || s.LLMProvider[0] != "anthropic" || s.LLMProvider[1] != "bedrock" {
		t.Fatalf("llm_provider = %v", s.LLMProvider)
	}
	t.Setenv("LLM_PROVIDER", "openai")
	if _, err := Load(""); err == nil {
		t.Fatal("want error for unknown provider")
	}
}

func TestLoadInvalid(t *testing.T) {
	for name, content := range map[string]string{
		"unknown field":    "server_prot: 1\n",
		"unknown provider": "llm_provider: [openai]\n",
		"unknown storage":  "storage:\n  default: s3\n",
		"missing env_file": "env_file: nope.env\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, t.TempDir(), "backend.yml", content)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
