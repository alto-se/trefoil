package build

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"strings"

	"trefoil/api/assets"
	"trefoil/api/internal/conf"
)

const defaultBaseImage = "python:3.12-slim"

// Context is a Docker build context: a Dockerfile plus the files it copies.
type Context struct {
	Dockerfile string
	Files      map[string][]byte
}

// agentSpec is written into the image as /trefoil/agent.json so the agent
// runtime has everything it needs without the original config.
type agentSpec struct {
	Name      string        `json:"name"`
	Model     string        `json:"model,omitempty"`
	Provider  string        `json:"provider,omitempty"`
	AWSRegion string        `json:"aws_region,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Limits    conf.Limits   `json:"limits,omitzero"`
	Bricks    []string      `json:"bricks"`
	Prompts   []conf.Prompt `json:"prompts,omitempty"`
	// JSON Schemas generated from the agent's input and outputs fields.
	InputSchema  map[string]any    `json:"input_schema,omitempty"`
	OutputSchema map[string]any    `json:"output_schema,omitempty"`
	Tools        []toolSpec        `json:"tools,omitempty"`
	ServerTools  []conf.ServerTool `json:"server_tools,omitempty"`
	Skills       []skillSpec       `json:"skills,omitempty"`
	MCPs         []conf.MCP        `json:"mcps,omitempty"`
}

type toolSpec struct {
	Brick       string         `json:"brick"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Language    string         `json:"language"`
	Entry       string         `json:"entry,omitempty"`
	Path        string         `json:"path"`
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// ImageTag returns the Docker tag for an agent name.
func ImageTag(name string) string {
	return "trefoil-agent-" + strings.Trim(unsafeChars.ReplaceAllString(strings.ToLower(name), "-"), "-.") + ":latest"
}

// AgentBricks returns the bricks an agent's image is built from:
// defaults.bricks first, then the agent's own, in order, without duplicates.
func AgentBricks(c *conf.Conf, a *conf.Agent) []string {
	return uniq(append(append([]string{}, c.Defaults.Bricks...), a.Bricks...))
}

// AgentSecrets returns the secrets declared by the bricks an agent uses. A
// secret is required if any brick requires it.
func AgentSecrets(c *conf.Conf, a *conf.Agent) map[string]conf.Secret {
	out := map[string]conf.Secret{}
	for _, name := range AgentBricks(c, a) {
		for k, s := range c.Bricks[name].Secrets {
			s.Required = s.Required || out[k].Required
			out[k] = s
		}
	}
	return out
}

// Compose turns an agent and the bricks it uses into a build context.
func Compose(c *conf.Conf, a *conf.Agent) (*Context, error) {
	bricks := AgentBricks(c, a)

	base := c.Defaults.BaseImage
	if base == "" {
		base = defaultBaseImage
	}

	var df strings.Builder
	fmt.Fprintf(&df, "# syntax=docker/dockerfile:1\n")
	fmt.Fprintf(&df, "FROM %s\n", base)
	// No TTY during builds; keeps apt from trying (and logging) interactive debconf frontends.
	df.WriteString("ARG DEBIAN_FRONTEND=noninteractive\n")

	spec := agentSpec{
		Name:      a.Name,
		Model:     c.Defaults.Model,
		Provider:  c.Defaults.Provider,
		AWSRegion: c.Defaults.AWSRegion,
		MaxTokens: c.Defaults.MaxTokens,
		Limits:    c.AgentLimits(a),
		Bricks:    bricks,
		Prompts:   a.Prompts,
	}
	if a.InputSchema != nil {
		spec.InputSchema = a.InputSchema.Map()
	}
	if len(a.Outputs) > 0 {
		spec.OutputSchema = a.Outputs.JSONSchema()
	}
	files := map[string][]byte{}
	skillBricks := map[string]string{} // skill name -> brick, to catch clashes
	var fetches []skillSpec

	for _, name := range bricks {
		b, ok := c.Bricks[name]
		if !ok {
			return nil, fmt.Errorf("agent %q uses unknown brick %q", a.Name, name)
		}

		for _, l := range b.Layers {
			fmt.Fprintf(&df, "\n# %s: %s\n", name, oneLine(l.Title))
			writeRun(&df, l.Script)
		}

		for _, t := range b.Tools {
			file := path.Join("tools", SafeName(name), SafeName(t.Name)+ext(t.Command.Language))
			files[file] = []byte(t.Command.Code)
			spec.Tools = append(spec.Tools, toolSpec{
				Brick:       name,
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
				Language:    t.Command.Language,
				Entry:       t.Command.Entry,
				Path:        "/trefoil/" + file,
			})
		}
		spec.ServerTools = append(spec.ServerTools, b.ServerTools...)
		for _, s := range b.Skills {
			sk := skillSpec{Brick: name, Name: s.Name, Source: s.Source}
			if packagedSkill(s) {
				sk.Source = ""
				skillName, skillFiles, err := skillFiles(s)
				if err != nil {
					return nil, fmt.Errorf("brick %q: %w", name, err)
				}
				maps.Copy(files, skillFiles)
				sk.Name = skillName
			} else {
				fetches = append(fetches, sk)
			}
			if other, ok := skillBricks[sk.Name]; ok && sk.Name != "" {
				return nil, fmt.Errorf("skill %q is in both brick %q and brick %q", sk.Name, other, name)
			}
			skillBricks[sk.Name] = name
			spec.Skills = append(spec.Skills, sk)
		}
		spec.MCPs = append(spec.MCPs, b.MCPs...)
	}

	if len(fetches) > 0 {
		df.WriteString("\n# skills from URLs\n")
		df.WriteString("COPY harness/skills.py /trefoil/harness/skills.py\n")
		for _, f := range fetches {
			args := []string{"python", "/trefoil/harness/skills.py", "fetch", f.Source}
			if f.Name != "" {
				args = append(args, f.Name)
			}
			run, _ := json.Marshal(args)
			fmt.Fprintf(&df, "RUN %s\n", run)
		}
	}

	specJSON, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	files["agent.json"] = specJSON
	files["harness/main.py"] = assets.Harness
	files["harness/skills.py"] = assets.Skills

	df.WriteString("\n# trefoil harness\n")
	if spec.Provider == "bedrock" {
		// The bedrock extra brings botocore for SigV4 signing.
		df.WriteString("RUN pip install --no-cache-dir 'anthropic[bedrock]>=2,<3'\n")
	} else {
		df.WriteString("RUN pip install --no-cache-dir 'anthropic>=2,<3'\n")
	}
	if len(spec.MCPs) > 0 {
		df.WriteString("RUN pip install --no-cache-dir 'mcp>=1.10,<2'\n")
		for _, m := range spec.MCPs {
			if m.SigV4 != nil {
				// botocore signs SigV4 MCP requests.
				df.WriteString("RUN pip install --no-cache-dir botocore\n")
				break
			}
		}
	}
	df.WriteString("COPY harness/ /trefoil/harness/\n")
	if len(spec.Tools) > 0 {
		df.WriteString("COPY tools/ /trefoil/tools/\n")
	}
	if len(spec.Skills) > len(fetches) {
		df.WriteString("COPY skills/ /trefoil/skills/\n")
	}
	df.WriteString("COPY agent.json /trefoil/agent.json\n")
	df.WriteString("ENV TREFOIL_AGENT=/trefoil/agent.json\n")
	df.WriteString("WORKDIR /workspace\n")
	df.WriteString(`CMD ["python", "/trefoil/harness/main.py"]` + "\n")

	return &Context{Dockerfile: df.String(), Files: files}, nil
}

// writeRun emits a RUN step, using a heredoc for multi-line scripts.
func writeRun(df *strings.Builder, script string) {
	script = strings.TrimSpace(script)
	if !strings.Contains(script, "\n") {
		fmt.Fprintf(df, "RUN %s\n", script)
		return
	}
	fmt.Fprintf(df, "RUN <<'TREFOIL_EOF'\nset -e\n%s\nTREFOIL_EOF\n", script)
}

func uniq(names []string) []string {
	seen := map[string]bool{}
	// Non-nil so an agent without bricks encodes as [] rather than null.
	out := []string{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// SafeName turns s into a lowercase name safe for a file or directory.
func SafeName(s string) string {
	s = strings.Trim(unsafeChars.ReplaceAllString(strings.ToLower(s), "_"), "_.")
	if s == "" {
		return "unnamed"
	}
	return s
}

func ext(language string) string {
	switch strings.ToLower(language) {
	case "python":
		return ".py"
	case "bash", "sh", "shell":
		return ".sh"
	case "javascript", "node":
		return ".js"
	default:
		return ""
	}
}
