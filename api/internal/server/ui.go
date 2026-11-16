package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"trefoil/api/internal/build"
	"trefoil/api/internal/conf"
	"trefoil/api/internal/jobs"
	"trefoil/api/internal/modules"
	"trefoil/api/internal/sessions"
)

// The dashboard is server-rendered HTML; htmx swaps in fragments and polls
// them so the page follows builds and runs as they happen.

//go:embed ui/*.html
var uiFiles embed.FS

var uiTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"ago": ago,
	"dur": dur,
}).ParseFS(uiFiles, "ui/*.html"))

func (s *server) registerUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.uiPage("page.html", nil))
	mux.HandleFunc("GET /ui/agents", s.handleUIAgents)
	mux.HandleFunc("GET /ui/jobs", s.handleUIJobs)
	mux.HandleFunc("GET /ui/jobs/{id}", s.handleUIJob)
	mux.HandleFunc("GET /ui/agents/{name}", s.handleUIAgent)
	mux.HandleFunc("GET /ui/agents/{name}/run", s.handleUIRunForm)
	mux.HandleFunc("POST /ui/agents/{name}/run", s.handleUIRun)
	mux.HandleFunc("GET /ui/sessions", s.handleUISessions)
	mux.HandleFunc("GET /ui/sessions/{id}", s.handleUISession)
	mux.HandleFunc("GET /ui/secrets", s.handleUISecrets)
	mux.HandleFunc("POST /ui/secrets", s.handleUISetSecret)
	mux.HandleFunc("GET /ui/params", s.handleUIParams)
	mux.HandleFunc("POST /ui/params", s.handleUISetParams)
	mux.HandleFunc("GET /ui/modules", s.handleUIModules)
}

func (s *server) uiPage(name string, data any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.render(w, name, data) }
}

func (s *server) handleUIAgents(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Agents []agentSummary
		Error  string
	}{}
	if c, _, err := s.readConf(r.Context()); err != nil {
		data.Error = err.Error()
	} else {
		data.Agents = s.agentSummaries(r.Context(), c)
	}
	s.render(w, "agents.html", data)
}

// agentView is one agent's page: the prompts and tools its harness gets,
// read from the agent.json a build would put in the image.
type agentView struct {
	Name     string
	Model    string
	Provider string
	Modules  []string
	Limits   conf.Limits
	Prompts  []conf.Prompt
	Tools    []agentTool
	MCPs     []conf.MCP
	Skills   []agentSkill
	Error    string
}

// agentTool is a tool the model can call. Kind is the tool's language,
// "server" for an Anthropic-hosted tool or "skills" for the harness's Skill
// tool; Schema is its input schema as indented JSON.
type agentTool struct {
	Name        string
	Module      string
	Kind        string
	Description string
	Schema      string
}

type agentSkill struct {
	Name        string
	Module      string
	Source      string
	Description string
}

func (s *server) handleUIAgent(w http.ResponseWriter, r *http.Request) {
	view := agentView{Name: r.PathValue("name")}
	if err := s.agentDetails(r.Context(), &view); err != nil {
		view.Error = err.Error()
	}
	s.render(w, "agent.html", view)
}

func (s *server) agentDetails(ctx context.Context, view *agentView) error {
	c, _, err := s.readConf(ctx)
	if err != nil {
		return err
	}
	a, err := c.Agent(view.Name)
	if err != nil {
		return err
	}
	bc, err := build.Compose(c, a)
	if err != nil {
		return err
	}
	var spec struct {
		Model    string        `json:"model"`
		Provider string        `json:"provider"`
		Modules  []string      `json:"modules"`
		Limits   conf.Limits   `json:"limits"`
		Prompts  []conf.Prompt `json:"prompts"`
		Tools    []struct {
			Module      string         `json:"module"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
			Language    string         `json:"language"`
		} `json:"tools"`
		ServerTools []conf.ServerTool `json:"server_tools"`
		Skills      []agentSkill      `json:"skills"`
		MCPs        []conf.MCP        `json:"mcps"`
	}
	if err := json.Unmarshal(bc.Files["agent.json"], &spec); err != nil {
		return err
	}
	view.Model, view.Provider, view.Modules, view.Limits = spec.Model, spec.Provider, spec.Modules, spec.Limits
	view.Prompts, view.MCPs, view.Skills = spec.Prompts, spec.MCPs, spec.Skills
	for _, t := range spec.Tools {
		view.Tools = append(view.Tools, agentTool{Name: t.Name, Module: t.Module, Kind: t.Language,
			Description: t.Description, Schema: prettyJSON(t.InputSchema)})
	}
	for _, t := range spec.ServerTools {
		name, _ := t["name"].(string)
		view.Tools = append(view.Tools, agentTool{Name: name, Kind: "server", Description: "Anthropic-hosted tool"})
	}
	for i := range view.Skills {
		view.Skills[i].Description = build.SkillDescription(bc, view.Skills[i].Name)
	}
	// The harness adds a Skill tool to load them; see load_skills in main.py.
	if len(view.Skills) > 0 {
		view.Tools = append(view.Tools, agentTool{Name: "Skill", Kind: "skills",
			Description: "Loads a skill's instructions (its SKILL.md), or another file from the skill's folder."})
	}
	return nil
}

func (s *server) handleUIJobs(w http.ResponseWriter, r *http.Request) {
	list := []jobs.Snapshot{}
	for _, j := range s.jobs.List() {
		list = append(list, j.Snapshot())
	}
	s.render(w, "jobs.html", list)
}

func (s *server) handleUIJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	s.render(w, "job.html", j.Snapshot())
}

// moduleRow is one module in the config, with the agents built from it.
type moduleRow struct {
	Name   string
	Shared bool
	Agents []string
	conf.Module
}

func (s *server) handleUIModules(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Modules []moduleRow
		Error   string
	}{}
	c, _, err := s.readConf(r.Context())
	if err != nil {
		data.Error = err.Error()
		s.render(w, "modules.html", data)
		return
	}
	users := map[string][]string{}
	for i := range c.Agents {
		for _, name := range build.AgentModules(c, &c.Agents[i]) {
			users[name] = append(users[name], c.Agents[i].Name)
		}
	}
	for name, b := range c.Modules {
		data.Modules = append(data.Modules, moduleRow{Name: name, Shared: modules.IsShared(name), Agents: users[name], Module: b})
	}
	sort.Slice(data.Modules, func(i, j int) bool { return data.Modules[i].Name < data.Modules[j].Name })
	s.render(w, "modules.html", data)
}

// secretRow is one secret the config's modules declare. Source is where its
// value comes from: "stored", "env" or "" when it has none.
type secretRow struct {
	Name     string
	Required bool
	Agents   []string
	Source   string
}

type secretsView struct {
	Secrets []secretRow
	// Saved names the secret just stored; Error is a failed save or read.
	Saved string
	Error string
}

func (s *server) handleUISecrets(w http.ResponseWriter, r *http.Request) {
	s.renderSecrets(w, r, secretsView{})
}

// handleUISetSecret stores a secret from the form and re-renders the list.
// Errors come back with 200 so htmx swaps them in.
func (s *server) handleUISetSecret(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretBytes)
	name := strings.TrimSpace(r.FormValue("name"))
	view := secretsView{}
	if _, err := s.storeSecret(r.Context(), name, r.FormValue("value")); err != nil {
		view.Error = fmt.Sprintf("%s: %v", name, err)
	} else {
		view.Saved = strings.ToUpper(name)
		// Agents waiting on this secret can run now.
		w.Header().Set("HX-Trigger", "refresh")
	}
	s.renderSecrets(w, r, view)
}

func (s *server) renderSecrets(w http.ResponseWriter, r *http.Request, view secretsView) {
	c, _, err := s.readConf(r.Context())
	if err == nil {
		view.Secrets, err = s.secretRows(r.Context(), c)
	}
	if err != nil && view.Error == "" {
		view.Error = err.Error()
	}
	s.render(w, "secrets.html", view)
}

// secretRows lists every secret any agent needs, keyed by the variable name
// the agent sees, so github_token and GITHUB_TOKEN are one row.
func (s *server) secretRows(ctx context.Context, c *conf.Conf) ([]secretRow, error) {
	rows := map[string]*secretRow{}
	for i := range c.Agents {
		a := &c.Agents[i]
		for name, sec := range build.AgentSecrets(c, a) {
			key := strings.ToUpper(name)
			row := rows[key]
			if row == nil {
				row = &secretRow{Name: key}
				rows[key] = row
			}
			row.Required = row.Required || sec.Required
			row.Agents = append(row.Agents, a.Name)
		}
	}
	out := make([]secretRow, 0, len(rows))
	for _, row := range rows {
		src, err := s.secretSource(ctx, row.Name)
		if err != nil {
			s.logger.Error("read secret", "secret", row.Name, "err", err)
			return nil, errors.New("failed to read secrets")
		}
		row.Source = src
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// paramsView is the dashboard's form for the config's params.
type paramsView struct {
	Fields []inputField
	Rows   []paramRow
	// Saved is set after a save; Error is a failed save or read.
	Saved bool
	Error string
}

func (s *server) handleUIParams(w http.ResponseWriter, r *http.Request) {
	s.renderParams(w, r, paramsView{}, nil)
}

// handleUISetParams stores every param from the form: blank fields are
// unset, and so are values equal to the param's default, so the param
// follows the config's default. Errors come back with 200 so htmx swaps
// them in, keeping what was typed.
func (s *server) handleUISetParams(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxParamBytes)
	_ = r.ParseForm()
	posted := func(name string) []string { return r.Form["field."+name] }
	c, _, err := s.readConf(r.Context())
	if err != nil {
		s.renderParams(w, r, paramsView{Error: err.Error()}, posted)
		return
	}
	var values map[string]any
	data, err := formInput(paramFields(c, posted, nil))
	if err == nil {
		err = json.Unmarshal(data, &values)
	}
	if err == nil {
		var unset []string
		for _, p := range c.Params.Props() {
			v, ok := values[p.Name]
			if ok && p.Default != nil {
				a, _ := json.Marshal(v)
				b, _ := json.Marshal(p.Default)
				ok = !bytes.Equal(a, b)
			}
			if !ok {
				delete(values, p.Name)
				unset = append(unset, p.Name)
			}
		}
		_, err = s.setParams(r.Context(), c, values, unset)
	}
	if err != nil {
		s.renderParams(w, r, paramsView{Error: err.Error()}, posted)
		return
	}
	// Run forms opened from now on start with the new values.
	w.Header().Set("HX-Trigger", "refresh")
	s.renderParams(w, r, paramsView{Saved: true}, nil)
}

// renderParams renders the params form, with posted values when it is
// re-shown after a failed save.
func (s *server) renderParams(w http.ResponseWriter, r *http.Request, view paramsView, posted func(string) []string) {
	c, _, err := s.readConf(r.Context())
	if err == nil {
		var params map[string]any
		if params, err = s.paramValues(r.Context(), c); err == nil {
			view.Fields = paramFields(c, posted, params)
			view.Rows, err = s.paramRows(r.Context(), c)
		}
	}
	if err != nil && view.Error == "" {
		view.Error = err.Error()
	}
	s.render(w, "params.html", view)
}

// paramFields builds the params form's controls, the same ones a run form
// uses for its input.
func paramFields(c *conf.Conf, posted func(string) []string, params map[string]any) []inputField {
	fields := inputFields(c.Params, posted, params)
	for i := range fields {
		fields[i].ID, fields[i].Autofocus, fields[i].Param = "param-"+fields[i].Name, false, false
	}
	return fields
}

// runForm is the dashboard's form for starting a run. Agents with an input
// schema get one control per property; others take free text in Input.
type runForm struct {
	Agent  string
	Fields []inputField
	Input  string
	Error  string
}

// inputField is one input property as a form control. Kind picks the control:
// text, integer, number, checkbox, select (an enum or an optional boolean),
// choices (a list of enum values, as checkboxes), lines (a list of scalars,
// one per line) or json (objects and arrays).
type inputField struct {
	Name        string
	Type        string
	Description string
	Kind        string
	Optional    bool
	Value       string
	Options     []inputOption
	Min, Max    string
	// ID is the control's element id; Autofocus is set on the first field.
	ID        string
	Autofocus bool
	// Param is set when a config param of the same name has a value, which
	// a fresh form starts with.
	Param bool
}

// inputOption is one choice of a select or choices control.
type inputOption struct {
	Value    string
	Selected bool
}

// inputFields builds the form controls for the schema. values returns what
// was posted for a property; nil values (a fresh form) use the property's
// value in params, else its default.
func inputFields(schema *conf.InputSchema, values func(string) []string, params map[string]any) []inputField {
	props := schema.Props()
	out := make([]inputField, 0, len(props))
	for i, f := range props {
		base, list, optional := f.Parts()
		enum := f.Enum
		if base == "boolean" && !list && optional && enum == nil {
			enum = []any{true, false}
		}
		kind := "json"
		switch {
		case list && enum != nil:
			kind = "choices"
		case list && base != "object" && base != "array":
			kind = "lines"
		case list:
		case enum != nil:
			kind = "select"
		case base == "string":
			kind = "text"
		case base == "integer" || base == "number":
			kind = base
		case base == "boolean":
			kind = "checkbox"
		}
		param, isParam := params[f.Name]
		var vals []string
		switch {
		case values != nil:
			vals = values(f.Name)
		case isParam:
			vals = formValues(param)
		case f.Default != nil:
			vals = formValues(f.Default)
		}
		field := inputField{Name: f.Name, Type: f.Type, Description: f.Description, Kind: kind,
			Optional: optional, Value: strings.Join(vals, "\n"),
			ID: "run-" + f.Name, Autofocus: i == 0, Param: isParam}
		for _, e := range enum {
			v := fmt.Sprint(e)
			field.Options = append(field.Options, inputOption{Value: v, Selected: slices.Contains(vals, v)})
		}
		if f.Minimum != nil {
			field.Min = strconv.FormatFloat(*f.Minimum, 'f', -1, 64)
		}
		if f.Maximum != nil {
			field.Max = strconv.FormatFloat(*f.Maximum, 'f', -1, 64)
		}
		out = append(out, field)
	}
	return out
}

// formValues renders a schema default as form values: one per list item,
// JSON for objects and nested lists.
func formValues(v any) []string {
	switch v := v.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			switch item.(type) {
			case map[string]any, []any:
				b, _ := json.MarshalIndent(v, "", "  ")
				return []string{string(b)}
			}
			out = append(out, fmt.Sprint(item))
		}
		return out
	case map[string]any:
		b, _ := json.MarshalIndent(v, "", "  ")
		return []string{string(b)}
	}
	return []string{fmt.Sprint(v)}
}

// formInput turns the posted form into the agent's JSON input. Blank
// optional fields are left out; InputSchema.Validate (via startRun) has the final
// say on types.
func formInput(fields []inputField) ([]byte, error) {
	input := map[string]any{}
	for _, f := range fields {
		raw := strings.TrimSpace(f.Value)
		if f.Kind == "checkbox" {
			input[f.Name] = raw == "true"
			continue
		}
		if raw == "" {
			// A required list may be empty; any other blank field is unset.
			if (f.Kind == "lines" || f.Kind == "choices") && !f.Optional {
				input[f.Name] = []any{}
			}
			continue
		}
		base, _, _ := conf.Field{Type: f.Type}.Parts()
		var err error
		switch f.Kind {
		case "lines", "choices":
			items := []any{}
			for _, line := range strings.Split(raw, "\n") {
				if line = strings.TrimSpace(line); line == "" {
					continue
				}
				v, lineErr := scalar(base, line)
				if lineErr != nil {
					err = fmt.Errorf("item %q: %w", line, lineErr)
					break
				}
				items = append(items, v)
			}
			input[f.Name] = items
		case "json":
			var v any
			if err = json.Unmarshal([]byte(raw), &v); err != nil {
				err = fmt.Errorf("not valid JSON: %w", err)
			}
			input[f.Name] = v
		default:
			input[f.Name], err = scalar(base, raw)
		}
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Name, err)
		}
	}
	return json.Marshal(input)
}

// scalar parses one form value as a string, number, integer or boolean.
func scalar(base, s string) (any, error) {
	switch base {
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, errors.New("want an integer")
		}
		return n, nil
	case "number":
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, errors.New("want a number")
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, errors.New("want true or false")
		}
		return b, nil
	}
	return s, nil
}

func (s *server) handleUIRunForm(w http.ResponseWriter, r *http.Request) {
	c, _, err := s.readConf(r.Context())
	if err == nil {
		var a *conf.Agent
		var params map[string]any
		if a, err = c.Agent(r.PathValue("name")); err == nil {
			if params, err = s.paramValues(r.Context(), c); err == nil {
				s.render(w, "run.html", runForm{Agent: a.Name, Fields: inputFields(a.InputSchema, nil, params)})
				return
			}
		}
	}
	s.render(w, "run.html", runForm{Agent: r.PathValue("name"), Error: err.Error()})
}

// handleUIRun starts a run and swaps the form for the live session. Errors
// re-render the form, keeping what was typed, with 200 so htmx swaps them in.
func (s *server) handleUIRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRunInputBytes)
	form := runForm{Agent: r.PathValue("name"), Input: r.FormValue("input")}
	c, _, err := s.readConf(r.Context())
	if err == nil {
		var a *conf.Agent
		if a, err = c.Agent(form.Agent); err == nil {
			params, _ := s.paramValues(r.Context(), c)
			form.Fields = inputFields(a.InputSchema, func(name string) []string { return r.Form["field."+name] }, params)
			input := []byte(form.Input)
			if len(form.Fields) > 0 {
				input, err = formInput(form.Fields)
			}
			if err == nil {
				var sess *sessions.Session
				if sess, _, err = s.startRun(r.Context(), c, a, input); err == nil {
					// Lists pick the new session up without waiting for the poll,
					// and the page moves to the run's own view.
					w.Header().Set("HX-Trigger", "refresh")
					w.Header().Set("HX-Redirect", "/#runs/"+sess.ID())
					s.renderSession(w, sess)
					return
				}
			}
		}
	}
	form.Error = err.Error()
	s.render(w, "run.html", form)
}

func (s *server) handleUISessions(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Runs  []runRow
		Usage usageChart
	}{Runs: []runRow{}}
	var tokens, costs []float64
	for _, sess := range s.sessions.List() {
		logs, _ := sess.Logs(0)
		events := sessions.ParseEvents(logs)
		row := newRunRow(sess.Status(), events)
		if row.Finished {
			tokens = append(tokens, row.Tokens)
			costs = append(costs, row.Cost)
		}
		data.Runs = append(data.Runs, row)
	}
	// List is newest first; the chart reads left to right.
	slices.Reverse(tokens)
	slices.Reverse(costs)
	data.Usage = newUsageChart(tokens, costs)
	s.render(w, "sessions.html", data)
}

// usageChart sums what finished runs spent, with a sparkline of each run's
// tokens and cost, oldest first. The lines are SVG polyline points in a
// usageW x usageH box.
type usageChart struct {
	Runs      int
	Tokens    float64
	Cost      float64
	TokenLine string
	CostLine  string
}

const usageW, usageH = 120.0, 28.0

func newUsageChart(tokens, costs []float64) usageChart {
	u := usageChart{Runs: len(tokens), TokenLine: sparkline(tokens), CostLine: sparkline(costs)}
	for i := range tokens {
		u.Tokens += tokens[i]
		u.Cost += costs[i]
	}
	return u
}

// runRow is one run in the list with its own usage: Line is a sparkline of
// tokens per model turn, so it grows while the run is going. Tokens counts
// those turns until the run reports its total; Finished is set once it has,
// and only then is Cost known.
type runRow struct {
	sessions.StatusInfo
	Turns    int
	Tokens   float64
	Cost     float64
	Finished bool
	Line     string
}

func newRunRow(info sessions.StatusInfo, events []sessions.Event) runRow {
	row := runRow{StatusInfo: info}
	var turns []float64
	for _, e := range events {
		if e.Type == "model" {
			in, _ := e.Data["input_tokens"].(float64)
			out, _ := e.Data["output_tokens"].(float64)
			turns = append(turns, in+out)
			row.Tokens += in + out
		}
	}
	row.Turns = len(turns)
	row.Line = sparkline(turns)
	if t, c, ok := runUsage(events); ok {
		row.Tokens, row.Cost, row.Finished = t, c, true
	}
	return row
}

// runUsage is the usage a run reported in its last done or limit event.
func runUsage(events []sessions.Event) (tokens, cost float64, ok bool) {
	for _, e := range slices.Backward(events) {
		if e.Type == "done" || e.Type == "limit" {
			tokens, _ = e.Data["tokens"].(float64)
			cost, _ = e.Data["cost_usd"].(float64)
			return tokens, cost, true
		}
	}
	return 0, 0, false
}

// sparkline scales values to polyline points, the largest touching the top.
// A single value is drawn as a flat line.
func sparkline(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 {
		values = []float64{values[0], values[0]}
	}
	top := slices.Max(values)
	var b strings.Builder
	for i, v := range values {
		y := usageH - 1
		if top > 0 {
			y -= v / top * (usageH - 2)
		}
		fmt.Fprintf(&b, "%.1f,%.1f ", float64(i)*usageW/float64(len(values)-1), y)
	}
	return strings.TrimSpace(b.String())
}

func (s *server) handleUISession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessions.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	s.renderSession(w, sess)
}

func (s *server) renderSession(w http.ResponseWriter, sess *sessions.Session) {
	logs, _ := sess.Logs(0)
	info := sess.Status()
	events := sessions.ParseEvents(logs)
	s.render(w, "session.html", struct {
		sessions.StatusInfo
		Logs      string
		Result    *sessions.Result
		Artifacts []sessions.Artifact
		Timeline  []timelineItem
		Budgets   []budgetRow
	}{info, sessions.StripEvents(logs), sess.Result(), sess.Artifacts(), timeline(info, events), budgets(info, events, logs)})
}

// budgetRow is one of a run's limits with how much of it the run has used.
// Pct is the bar width, capped at 100; an empty Limit means there is none and
// no bar. Level is a CSS class: running, finished or over (the run stopped
// at this limit).
type budgetRow struct {
	Name  string
	Used  string
	Limit string
	Pct   float64
	Level string
}

// budgets compares a run's limits with its usage so far. Tokens and cost
// come from the latest event that reports running totals; harnesses built
// before model events carried them only report cost when the run ends.
// Cost is always shown, with a bar when there is a max_cost_usd.
func budgets(info sessions.StatusInfo, events []sessions.Event, logs string) []budgetRow {
	l := info.Limits
	if l == (conf.Limits{}) {
		l = loggedLimits(logs)
	}
	var turns, tokens, cost float64
	costKnown, stopped := false, info.TimedOut
	for _, e := range events {
		switch e.Type {
		case "model", "done", "limit":
			stopped = stopped || e.Type == "limit"
			if e.Type == "model" {
				turns++
			}
			if t, ok := e.Data["tokens"].(float64); ok {
				tokens = t
				cost, costKnown = e.Data["cost_usd"].(float64)
			} else if e.Type == "model" {
				in, _ := e.Data["input_tokens"].(float64)
				out, _ := e.Data["output_tokens"].(float64)
				tokens += in + out
			}
		}
	}
	// Harnesses built before timeline events only log a summary at the end.
	if len(events) == 0 {
		if m := loggedUsage.FindStringSubmatch(logs); m != nil {
			turns, _ = strconv.ParseFloat(m[1], 64)
			tokens, _ = strconv.ParseFloat(m[2], 64)
			cost, _ = strconv.ParseFloat(m[3], 64)
			costKnown = true
		}
	}
	end := time.Now()
	if info.FinishedAt != nil {
		end = *info.FinishedAt
	}
	elapsed := end.Sub(info.CreatedAt).Seconds()

	var rows []budgetRow
	add := func(name string, used, limit float64, usedStr, limitStr string) {
		row := budgetRow{Name: name, Used: usedStr, Limit: limitStr, Level: "finished"}
		if info.Status == sessions.Running {
			row.Level = "running"
		}
		if limit > 0 {
			row.Pct = min(used/limit*100, 100)
			if stopped && used >= limit {
				row.Level = "over"
			}
		}
		rows = append(rows, row)
	}
	if l.MaxTurns > 0 {
		add("Turns", turns, float64(l.MaxTurns), fmt.Sprintf("%.0f", turns), fmt.Sprint(l.MaxTurns))
	}
	if l.TokenBudget > 0 {
		add("Tokens", tokens, float64(l.TokenBudget), fmt.Sprintf("%.0f", tokens), fmt.Sprint(l.TokenBudget))
	}
	used := fmt.Sprintf("≈$%.4f", cost)
	if !costKnown {
		used = "?"
	}
	if l.MaxCostUSD > 0 {
		add("Cost", cost, l.MaxCostUSD, used, fmt.Sprintf("$%.2f", l.MaxCostUSD))
	} else {
		add("Cost", cost, 0, used, "")
	}
	if l.TimeoutSeconds > 0 {
		add("Time", elapsed, l.TimeoutSeconds, (time.Duration(elapsed) * time.Second).String(),
			(time.Duration(l.TimeoutSeconds * float64(time.Second))).String())
	}
	return rows
}

// loggedUsage matches the harness's closing line, "[harness] done in 3
// turns; tokens=900 cost≈$0.0123".
var loggedUsage = regexp.MustCompile(`\[harness\] done in (\d+) turns; tokens=(\d+) cost≈\$([\d.]+)`)

// loggedLimits reads the limits from the harness's start line,
// "[harness] ... limits={'max_turns': 8}", for runs from before sessions
// stored them.
func loggedLimits(logs string) conf.Limits {
	var l conf.Limits
	for line := range strings.Lines(logs) {
		_, dict, ok := strings.Cut(line, " limits=")
		if !ok || !strings.HasPrefix(line, "[harness] agent=") {
			continue
		}
		_ = json.Unmarshal([]byte(strings.ReplaceAll(strings.TrimSpace(dict), "'", `"`)), &l)
		break
	}
	return l
}

// timelineItem is one row of a run's timeline. Status is a CSS class:
// succeeded, failed, running or empty. Input and Output are shown folded.
type timelineItem struct {
	Kind    string
	ID      string
	Offset  string
	Title   string
	Summary string
	Status  string
	Took    string
	Input   string
	Output  string
}

// timeline turns a run's events into rows, pairing each tool call with its
// result, between the server's own started and finished rows.
func timeline(info sessions.StatusInfo, events []sessions.Event) []timelineItem {
	offset := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return "+" + max(t.Sub(info.CreatedAt), 0).Round(100*time.Millisecond).String()
	}
	str := func(d map[string]any, key string) string {
		v, _ := d[key].(string)
		return v
	}
	num := func(d map[string]any, key string) float64 {
		v, _ := d[key].(float64)
		return v
	}
	items := []timelineItem{{Kind: "session", Offset: "+0s", Title: "Run started", Summary: info.Agent}}
	calls := map[string]int{}
	for _, e := range events {
		d := e.Data
		item := timelineItem{Kind: e.Type, Offset: offset(e.At)}
		switch e.Type {
		case "start":
			item.Title = "Harness started"
			item.Summary = str(d, "model")
			if tools := listOf(d["tools"]); len(tools) > 0 {
				item.Summary += " · tools: " + strings.Join(tools, ", ")
			}
		case "model":
			item.Title = fmt.Sprintf("Model · turn %v", d["turn"])
			item.Summary = fmt.Sprintf("%s · %.0f in / %.0f out tokens", str(d, "stop_reason"), num(d, "input_tokens"), num(d, "output_tokens"))
			item.Output = str(d, "text")
		case "tool_call", "server_tool":
			item.Kind = "tool"
			item.ID = str(d, "id")
			item.Title = str(d, "tool")
			item.Summary = callSummary(d["input"])
			item.Input = callInput(d["input"])
			item.Status = "running"
			if e.Type == "server_tool" {
				item.Title += " (server)"
				item.Status = ""
			}
			if item.ID != "" {
				calls[item.ID] = len(items)
			}
		case "tool_result":
			i, ok := calls[str(d, "id")]
			if !ok {
				continue
			}
			call := &items[i]
			call.Status, call.Output = "succeeded", str(d, "output")
			if d["is_error"] == true {
				call.Status = "failed"
			}
			call.Took = (time.Duration(num(d, "ms")) * time.Millisecond).String()
			continue
		case "done":
			item.Title = "Answered"
			item.Summary = fmt.Sprintf("%.0f turns · %.0f tokens · ≈$%.4f", num(d, "turns"), num(d, "tokens"), num(d, "cost_usd"))
			item.Status = "succeeded"
		case "limit":
			item.Title, item.Summary, item.Status = "Limit reached", str(d, "reason"), "failed"
		case "error":
			item.Title, item.Summary, item.Status = "Error", str(d, "message"), "failed"
		default:
			item.Title = e.Type
			item.Output = prettyJSON(d)
		}
		items = append(items, item)
	}
	if info.FinishedAt != nil {
		end := timelineItem{Kind: "session", Offset: offset(*info.FinishedAt), Title: "Run " + string(info.Status), Status: string(info.Status), Summary: info.Error}
		if info.ExitCode != nil {
			end.Summary = strings.TrimPrefix(fmt.Sprintf("%s · exit %d", info.Error, *info.ExitCode), " · ")
		}
		items = append(items, end)
		// A tool still running when the run ended has no result coming.
		for i := range items {
			if items[i].Status == "running" {
				items[i].Status = "failed"
				items[i].Took = "no result"
			}
		}
	}
	return items
}

func listOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// callSummary is a one-line view of a tool's input: key=value pairs with
// long values cut short.
func callSummary(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		var val string
		if str, ok := m[k].(string); ok {
			val = `"` + strings.Join(strings.Fields(str), " ") + `"`
		} else {
			val = jsonText(m[k], false)
		}
		if r := []rune(val); len(r) > 60 {
			val = string(r[:57]) + "…"
		}
		parts = append(parts, k+"="+val)
	}
	return strings.Join(parts, ", ")
}

// callInput shows a tool call's input for reading: one "key: value" per
// field, strings as plain text (a multi-line one indented on the lines
// below its key) and anything else as JSON.
func callInput(input any) string {
	m, ok := input.(map[string]any)
	if !ok {
		return prettyJSON(input)
	}
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		str, isStr := m[k].(string)
		if !isStr {
			str = jsonText(m[k], true)
		}
		if strings.Contains(str, "\n") {
			fmt.Fprintf(&b, "%s:\n  %s\n", k, strings.ReplaceAll(strings.TrimRight(str, "\n"), "\n", "\n  "))
		} else {
			fmt.Fprintf(&b, "%s: %s\n", k, str)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func prettyJSON(v any) string {
	if v == nil {
		return ""
	}
	return jsonText(v, true)
}

// jsonText marshals v without escaping &, < and > (the templates escape
// HTML), indented if asked.
func jsonText(v any, indent bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// render executes into a buffer first so a template error doesn't leave a
// half-written fragment in the page.
func (s *server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := uiTemplates.ExecuteTemplate(&buf, name, data); err != nil {
		s.logger.Error("render", "template", name, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("2006-01-02 15:04")
}

// dur is how long something has run: until end, or until now while running.
func dur(start time.Time, end *time.Time) string {
	stop := time.Now()
	if end != nil {
		stop = *end
	}
	return stop.Sub(start).Round(100 * time.Millisecond).String()
}
