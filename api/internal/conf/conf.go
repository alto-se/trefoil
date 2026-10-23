package conf

import (
	"encoding/json"
	"fmt"
)

// Conf is the packaged config accepted by POST /conf.
type Conf struct {
	Bricks   map[string]Brick `json:"bricks"`
	Agents   []Agent          `json:"agents,omitempty"`
	Defaults Defaults         `json:"defaults"`
	// Params declares config-wide values, written like an input_schema.
	// Their values are set on the server (POST /param/{name} or the
	// dashboard), falling back to each property's default; a run that
	// leaves out an input field named after a param gets the param's value.
	Params *InputSchema `json:"params,omitempty"`
}

type Brick struct {
	Source        string            `json:"source,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Skills        []Skill           `json:"skills,omitempty"`
	Layers        []Layer           `json:"layers,omitempty"`
	Tools         []Tool            `json:"tools,omitempty"`
	ServerTools   []ServerTool      `json:"server_tools,omitempty"`
	Secrets       map[string]Secret `json:"secrets,omitempty"`
	MCPs          []MCP             `json:"mcps,omitempty"`
}

// ZipDataPrefix starts a skill source that is a zip packaged into the config
// as base64 (a `file://...zip` ref), unpacked into the build context.
const ZipDataPrefix = "data:application/zip;base64,"

// Skill is an Agent Skill: a folder with a SKILL.md and any files it uses.
// Source is a URL to a zip of the folder, fetched at build time, a zip
// packaged as a base64 data: URL, or the folder itself, packaged as a map of
// relative path to file contents:
//
//	skills:
//	  - source: file://skills/release-notes.zip
//	  - source: file://skills/release-notes
//	  - source: https://example.com/skill.zip
//	    name: browser
type Skill struct {
	// Name is the folder the skill is installed as; empty means the name in
	// its SKILL.md (or, for a URL, the archive's file name).
	Name string
	// Source is the URL or packaged zip; Files the packaged folder. One of
	// them is set.
	Source string
	Files  map[string]string
}

// MarshalJSON writes a packaged folder back as source, the way it's read.
func (s Skill) MarshalJSON() ([]byte, error) {
	var source any = s.Source
	if s.Files != nil {
		source = s.Files
	}
	return json.Marshal(struct {
		Name   string `json:"name,omitempty"`
		Source any    `json:"source"`
	}{s.Name, source})
}

// UnmarshalJSON accepts source as a URL string or as a packaged folder.
func (s *Skill) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name   string          `json:"name"`
		Source json.RawMessage `json:"source"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Name = raw.Name
	if len(raw.Source) == 0 || json.Unmarshal(raw.Source, &s.Source) == nil {
		return nil
	}
	if err := json.Unmarshal(raw.Source, &s.Files); err != nil {
		return fmt.Errorf("source: want a URL or a folder of files")
	}
	return nil
}

type Layer struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Script      string `json:"script"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	Command     Command        `json:"command"`
}

// ServerTool is an Anthropic-hosted tool (web_search, web_fetch). Options
// besides name are passed through to the API tool definition as-is.
type ServerTool map[string]any

func (t ServerTool) Name() string {
	name, _ := t["name"].(string)
	return name
}

type Command struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	// Entry is the function to call in Code; empty means the tool's name.
	Entry string `json:"entry,omitempty"`
}

// UnmarshalJSON accepts code as a string, or as {content, entry} so several
// tools can share one file:
//
//	code: {content: !Ref file://tools.py, entry: Write}
func (c *Command) UnmarshalJSON(data []byte) error {
	var raw struct {
		Language string          `json:"language"`
		Code     json.RawMessage `json:"code"`
		Entry    string          `json:"entry"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Language, c.Entry = raw.Language, raw.Entry
	if len(raw.Code) == 0 || json.Unmarshal(raw.Code, &c.Code) == nil {
		return nil
	}
	var code struct {
		Content string `json:"content"`
		Entry   string `json:"entry"`
	}
	if err := json.Unmarshal(raw.Code, &code); err != nil {
		return fmt.Errorf("code: want a string or {content, entry}")
	}
	c.Code = code.Content
	if code.Entry != "" {
		c.Entry = code.Entry
	}
	return nil
}

type Secret struct {
	Required bool `json:"required,omitempty"`
}

type MCP struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Credentials map[string]string `json:"credentials,omitempty"`
	// SigV4 signs every request with the container's AWS credentials, for
	// MCP servers behind AWS IAM such as the AWS MCP Server.
	SigV4 *SigV4 `json:"sigv4,omitempty"`
}

// SigV4 names the AWS service and region requests are signed for.
type SigV4 struct {
	Service string `json:"service"`
	Region  string `json:"region"`
}

// Limits cap a single agent run. Zero means no limit.
type Limits struct {
	MaxTurns       int     `json:"max_turns,omitempty"`
	TokenBudget    int     `json:"token_budget,omitempty"`
	TimeoutSeconds float64 `json:"timeout_seconds,omitempty"`
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty"`
}

// Merge returns l with every unset field taken from fallback.
func (l Limits) Merge(fallback Limits) Limits {
	if l.MaxTurns == 0 {
		l.MaxTurns = fallback.MaxTurns
	}
	if l.TokenBudget == 0 {
		l.TokenBudget = fallback.TokenBudget
	}
	if l.TimeoutSeconds == 0 {
		l.TimeoutSeconds = fallback.TimeoutSeconds
	}
	if l.MaxCostUSD == 0 {
		l.MaxCostUSD = fallback.MaxCostUSD
	}
	return l
}

type Agent struct {
	Name   string   `json:"name"`
	Limits Limits   `json:"limits,omitzero"`
	Bricks []string `json:"bricks,omitempty"`
	// InputSchema, if set, makes runs take a JSON object matching it
	// instead of a free-form task.
	InputSchema *InputSchema `json:"input_schema,omitempty"`
	Outputs     Fields       `json:"outputs,omitempty"`
	Prompts     []Prompt     `json:"prompts,omitempty"`
}

type Prompt struct {
	Source string `json:"source"`
	Role   string `json:"role,omitempty"`
}

type Defaults struct {
	Bricks    []string `json:"bricks,omitempty"`
	Model     string   `json:"model,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	AWSRegion string   `json:"aws_region,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	BaseImage string   `json:"base_image,omitempty"`
	Limits    Limits   `json:"limits,omitzero"`
}

func Parse(data []byte) (*Conf, error) {
	var c Conf
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks rules the JSON Schema can't express.
func (c *Conf) Validate() error {
	seen := map[string]bool{}
	for _, a := range c.Agents {
		if seen[a.Name] {
			return fmt.Errorf("duplicate agent name %q", a.Name)
		}
		seen[a.Name] = true
		if err := a.Outputs.Check(true); err != nil {
			return fmt.Errorf("agent %q outputs: %w", a.Name, err)
		}
	}
	return nil
}

// Agent finds an agent by name.
func (c *Conf) Agent(name string) (*Agent, error) {
	for i := range c.Agents {
		if c.Agents[i].Name == name {
			return &c.Agents[i], nil
		}
	}
	return nil, fmt.Errorf("agent %q not found", name)
}

// BrickRefs returns every brick name used by defaults and agents, in order,
// without duplicates.
func (c *Conf) BrickRefs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(c.Defaults.Bricks)
	for _, a := range c.Agents {
		add(a.Bricks)
	}
	return out
}

// ParamValues returns each declared param's value: the one in set (stored
// on the server), else the property's default. Params with neither are
// left out, as are values in set for params no longer declared.
func (c *Conf) ParamValues(set map[string]any) map[string]any {
	out := map[string]any{}
	for _, p := range c.Params.Props() {
		if v, ok := set[p.Name]; ok {
			out[p.Name] = v
		} else if p.Default != nil {
			out[p.Name] = p.Default
		}
	}
	return out
}

// FillParams sets each of the agent's input properties missing from input
// to the param of the same name in values (see ParamValues). It reports
// whether it set any, and the required params it needed that have no value.
func (c *Conf) FillParams(a *Agent, input, values map[string]any) (filled bool, missing []string) {
	params := map[string]Prop{}
	for _, p := range c.Params.Props() {
		params[p.Name] = p
	}
	for _, f := range a.InputSchema.Props() {
		p, declared := params[f.Name]
		if _, set := input[f.Name]; set || !declared {
			continue
		}
		if v, ok := values[f.Name]; ok {
			input[f.Name] = v
			filled = true
		} else if _, _, optional := p.Parts(); !optional {
			missing = append(missing, f.Name)
		}
	}
	return filled, missing
}

// AgentLimits returns an agent's limits, with unset fields taken from
// defaults.limits.
func (c *Conf) AgentLimits(a *Agent) Limits {
	return a.Limits.Merge(c.Defaults.Limits)
}
