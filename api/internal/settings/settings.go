// Package settings loads the API server's own settings (backend.yml):
// defaults, then the YAML file, then environment overrides (ADDR, DATA_DIR,
// BRICKS_DIR).
package settings

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Providers are the LLM providers the agent harness supports.
var Providers = []string{"anthropic", "bedrock"}

type Settings struct {
	// EnvFile is a KEY=VALUE file loaded into the server's environment;
	// variables already set win. Agent containers get AWS_* and
	// ANTHROPIC_* variables forwarded from that environment.
	EnvFile string `yaml:"env_file"`
	AWS     AWS    `yaml:"aws"`
	// LLMProvider lists the providers configs may use. The first one is
	// the default for configs that don't set defaults.provider.
	LLMProvider []string `yaml:"llm_provider"`
	Storage     Storage  `yaml:"storage"`
	ServerPort  int      `yaml:"server_port"`
	BricksDir   string   `yaml:"bricks_dir"`

	// Addr is derived from ServerPort, or set by ADDR.
	Addr string `yaml:"-"`
}

type AWS struct {
	// Region is set as AWS_REGION in every agent container.
	Region string `yaml:"region"`
}

type Storage struct {
	Default string       `yaml:"default"`
	Local   LocalStorage `yaml:"local"`
}

type LocalStorage struct {
	Path string `yaml:"path"`
}

func Defaults() Settings {
	return Settings{
		Storage:    Storage{Default: "local", Local: LocalStorage{Path: "./data"}},
		ServerPort: 8080,
		BricksDir:  "./bricks",
	}
}

// Load reads path (skipped when empty) over the defaults, loads its
// env_file, then applies environment overrides.
func Load(path string) (Settings, error) {
	s := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return s, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&s); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
		// Relative paths in the file are relative to the file, not the cwd.
		for _, p := range []*string{&s.EnvFile, &s.Storage.Local.Path, &s.BricksDir} {
			if *p != "" && !filepath.IsAbs(*p) {
				*p = filepath.Join(filepath.Dir(path), *p)
			}
		}
	}
	if err := s.validate(); err != nil {
		return s, err
	}
	if s.EnvFile != "" {
		if err := loadEnvFile(s.EnvFile); err != nil {
			return s, err
		}
	}

	s.Addr = ":" + strconv.Itoa(s.ServerPort)
	for env, field := range map[string]*string{"ADDR": &s.Addr, "DATA_DIR": &s.Storage.Local.Path, "BRICKS_DIR": &s.BricksDir} {
		if v := os.Getenv(env); v != "" {
			*field = v
		}
	}
	return s, nil
}

func (s Settings) validate() error {
	if s.Storage.Default != "local" {
		return fmt.Errorf("storage.default: unsupported storage %q (only \"local\")", s.Storage.Default)
	}
	for _, p := range s.LLMProvider {
		if !slices.Contains(Providers, p) {
			return fmt.Errorf("llm_provider: unknown provider %q (want one of %s)", p, strings.Join(Providers, ", "))
		}
	}
	if s.ServerPort < 1 || s.ServerPort > 65535 {
		return fmt.Errorf("server_port: %d out of range", s.ServerPort)
	}
	return nil
}

// AgentEnv returns the variables set in every agent container.
func (s Settings) AgentEnv() map[string]string {
	env := map[string]string{}
	if s.AWS.Region != "" {
		env["AWS_REGION"] = s.AWS.Region
	}
	return env
}

// loadEnvFile sets KEY=VALUE lines from path that aren't already set.
// Blank lines, # comments, "export " prefixes and surrounding quotes are
// handled; nothing fancier.
func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("env_file: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			return fmt.Errorf("env_file %s:%d: want KEY=VALUE", path, n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	return sc.Err()
}
