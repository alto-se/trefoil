package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"trefoil/api/internal/build"
	"trefoil/api/internal/conf"
)

// exportFile is the manifest at the root of an environment export.
const exportFile = "environment.json"

// environment describes what running the exported agents needs outside
// trefoil: the LLM backend's variables, the secrets and params, and per
// agent how to build and run its image. It never holds secret values.
type environment struct {
	Provider  string `json:"provider"`
	Model     string `json:"model,omitempty"`
	AWSRegion string `json:"aws_region,omitempty"`
	// LLMEnv are the variables each container needs to reach the model.
	LLMEnv  []envVar        `json:"llm_env"`
	Secrets []exportSecret  `json:"secrets"`
	Params  []paramRow      `json:"params"`
	Agents  []exportedAgent `json:"agents"`
	// ArtifactsDir is where agents leave files for the user; the run
	// commands mount ./artifacts there.
	ArtifactsDir string `json:"artifacts_dir"`
}

type envVar struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

type exportSecret struct {
	Name     string   `json:"name"`
	Required bool     `json:"required"`
	Agents   []string `json:"agents"`
}

type exportedAgent struct {
	Name string `json:"name"`
	// Context is the folder in the export holding the agent's Dockerfile
	// and build context.
	Context     string            `json:"context"`
	Image       string            `json:"image"`
	Modules     []string          `json:"modules"`
	Limits      conf.Limits       `json:"limits,omitzero"`
	InputSchema *conf.InputSchema `json:"input_schema,omitempty"`
	Outputs     conf.Fields       `json:"outputs,omitempty"`
	// Secrets and Params are names from the top-level lists the agent uses.
	Secrets []string    `json:"secrets"`
	Params  []string    `json:"params"`
	MCPs    []exportMCP `json:"mcps,omitempty"`
	// Env is set in the container besides the secrets and LLM variables.
	Env   map[string]string `json:"env,omitempty"`
	Build string            `json:"build"`
	Run   string            `json:"run"`
}

// exportMCP is an MCP server an agent connects to. Credentials are left
// out; they expand from the agent's secrets at run time.
type exportMCP struct {
	Name  string      `json:"name"`
	URL   string      `json:"url"`
	SigV4 *conf.SigV4 `json:"sigv4,omitempty"`
}

// handleExport sends every agent's build context plus environment.json as
// trefoil-environment.zip, so the whole config can be built and run
// without the server.
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	env, err := s.environment(r, c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := range c.Agents {
		a := &c.Agents[i]
		bc, err := build.Compose(c, a)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("agent %q: %v", a.Name, err))
			return
		}
		if err := bc.AddToZip(zw, env.Agents[i].Context); err != nil {
			s.logger.Error("zip build context", "agent", a.Name, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to zip build context")
			return
		}
	}
	manifest, _ := json.MarshalIndent(env, "", "  ")
	if err := build.ZipFile(zw, exportFile, append(manifest, '\n')); err != nil || zw.Close() != nil {
		writeError(w, http.StatusInternalServerError, "failed to write export")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="trefoil-environment.zip"`)
	_, _ = w.Write(buf.Bytes())
}

func (s *server) environment(r *http.Request, c *conf.Conf) (*environment, error) {
	params, err := s.paramRows(r.Context(), c)
	if err != nil {
		return nil, err
	}
	secrets, err := s.secretRows(r.Context(), c)
	if err != nil {
		return nil, err
	}
	env := &environment{
		Provider:     c.Defaults.Provider,
		Model:        c.Defaults.Model,
		AWSRegion:    c.Defaults.AWSRegion,
		Params:       params,
		Secrets:      []exportSecret{},
		Agents:       []exportedAgent{},
		ArtifactsDir: build.ArtifactsDir,
	}
	if env.Provider == "" {
		env.Provider = "anthropic"
	}
	env.LLMEnv = llmEnv(env.Provider, env.AWSRegion != "")
	for _, row := range secrets {
		env.Secrets = append(env.Secrets, exportSecret{Name: row.Name, Required: row.Required, Agents: row.Agents})
	}
	declared := map[string]bool{}
	for _, p := range params {
		declared[p.Name] = true
	}
	for i := range c.Agents {
		a := &c.Agents[i]
		dir := "agents/" + build.SafeName(a.Name)
		ea := exportedAgent{
			Name:        a.Name,
			Context:     dir,
			Image:       build.ImageTag(a.Name),
			Modules:     build.AgentModules(c, a),
			Limits:      c.AgentLimits(a),
			InputSchema: a.InputSchema,
			Outputs:     a.Outputs,
			Secrets:     []string{},
			Params:      []string{},
		}
		for name := range build.AgentSecrets(c, a) {
			ea.Secrets = append(ea.Secrets, strings.ToUpper(name))
		}
		sort.Strings(ea.Secrets)
		for _, f := range a.InputSchema.Props() {
			if declared[f.Name] {
				ea.Params = append(ea.Params, f.Name)
			}
		}
		for _, name := range ea.Modules {
			for _, m := range c.Modules[name].MCPs {
				ea.MCPs = append(ea.MCPs, exportMCP{Name: m.Name, URL: m.URL, SigV4: m.SigV4})
			}
		}
		if ea.Limits != (conf.Limits{}) {
			limits, _ := json.Marshal(ea.Limits)
			ea.Env = map[string]string{"TREFOIL_LIMITS": string(limits)}
		}
		ea.Build = fmt.Sprintf("docker build -t %s %s", ea.Image, dir)
		ea.Run = runCommand(ea, env.LLMEnv)
		env.Agents = append(env.Agents, ea)
	}
	return env, nil
}

// llmEnv lists the variables the harness reads for provider, as the server
// forwards them in build.CLI.Run.
func llmEnv(provider string, regionInConfig bool) []envVar {
	if provider == "bedrock" {
		return []envVar{
			{Name: "AWS_REGION", Required: !regionInConfig, Description: "Bedrock region (defaults.aws_region wins when set)"},
			{Name: "AWS_ACCESS_KEY_ID", Description: "static or temporary AWS credentials, or AWS_BEARER_TOKEN_BEDROCK"},
			{Name: "AWS_SECRET_ACCESS_KEY"},
			{Name: "AWS_SESSION_TOKEN", Description: "with temporary credentials"},
			{Name: "AWS_BEARER_TOKEN_BEDROCK", Description: "Bedrock API key, instead of AWS credentials"},
		}
	}
	return []envVar{
		{Name: "ANTHROPIC_API_KEY", Required: true},
		{Name: "ANTHROPIC_BASE_URL", Description: "override the API endpoint"},
	}
}

// runCommand is a docker run line for the agent that forwards secrets and
// LLM variables from the caller's environment and reads the task or input
// JSON on stdin, with artifacts landing in ./artifacts.
func runCommand(a exportedAgent, llm []envVar) string {
	args := []string{fmt.Sprintf(`docker run -i --rm -v "$PWD/artifacts:%s"`, build.ArtifactsDir)}
	for k, v := range a.Env {
		args = append(args, fmt.Sprintf("-e %s='%s'", k, v))
	}
	for _, v := range llm {
		args = append(args, "-e "+v.Name)
	}
	for _, name := range a.Secrets {
		args = append(args, "-e "+name)
	}
	return strings.Join(append(args, a.Image, "< input.json"), " ")
}
