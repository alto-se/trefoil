package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"trefoil/api/internal/conf"
	"trefoil/api/internal/sessions"
)

func TestUI(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)

	rec := do(h, http.MethodGet, "/", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `hx-get="/ui/agents"`) {
		t.Fatalf("GET /: status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodGet, "/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope: status = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/ui/agents", ""); !strings.Contains(rec.Body.String(), "no config uploaded") {
		t.Fatalf("agents before conf: %s", rec.Body)
	}

	conf := `{"modules":{"os":{}},"defaults":{"modules":["os"],"model":"claude-sonnet-5"},"agents":[{"name":"echo"}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	job := buildAndWait(t, h, "/agents/echo/build")

	agents := do(h, http.MethodGet, "/ui/agents", "").Body.String()
	for _, want := range []string{"<strong>echo</strong>", "claude-sonnet-5", "pill built"} {
		if !strings.Contains(agents, want) {
			t.Errorf("agents fragment missing %q:\n%s", want, agents)
		}
	}
	if jobs := do(h, http.MethodGet, "/ui/jobs", "").Body.String(); !strings.Contains(jobs, "/ui/jobs/"+job["id"].(string)) {
		t.Errorf("jobs fragment missing job:\n%s", jobs)
	}
	if rec := do(h, http.MethodGet, "/ui/jobs/"+job["id"].(string), ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pill succeeded") {
		t.Errorf("job detail: status = %d\n%s", rec.Code, rec.Body)
	}

	rec = do(h, http.MethodPost, "/agents/echo/run", "hi")
	var run map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &run)
	id, _ := run["session_id"].(string)
	if id == "" {
		t.Fatalf("run: status = %d, body = %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		detail := do(h, http.MethodGet, "/ui/sessions/"+id, "").Body.String()
		if !strings.Contains(detail, "pill running") {
			if !strings.Contains(detail, "[harness] running") {
				t.Errorf("session detail missing logs:\n%s", detail)
			}
			if !strings.Contains(detail, `hx-post="/ui/sessions/`+id+`/rerun"`) || !strings.Contains(detail, "<h2>Input</h2>") {
				t.Errorf("session detail missing rerun button or input:\n%s", detail)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sessions := do(h, http.MethodGet, "/ui/sessions", "").Body.String(); !strings.Contains(sessions, "#runs/"+id) {
		t.Errorf("sessions fragment missing session:\n%s", sessions)
	}
	if rec := do(h, http.MethodGet, "/ui/sessions/missing", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing session: status = %d, want 404", rec.Code)
	}
	rec = do(h, http.MethodPost, "/ui/sessions/"+id+"/rerun", "")
	if next := strings.TrimPrefix(rec.Header().Get("HX-Redirect"), "/#runs/"); next == "" || next == id {
		t.Errorf("rerun: status = %d, HX-Redirect = %q, body = %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	if rec := do(h, http.MethodPost, "/ui/sessions/missing/rerun", ""); rec.Code != http.StatusNotFound {
		t.Errorf("rerun missing session: status = %d, want 404", rec.Code)
	}
}

func TestUIRun(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	conf := `{"modules":{"os":{}},"defaults":{"modules":["os"]},"agents":[{"name":"echo"},` +
		`{"name":"adder","input_schema":{"type":"object","properties":{` +
		`"a":{"type":"integer"},"label":{"type":"string","description":"Shown on top"},"loud":{"type":"boolean"},` +
		`"tags":{"type":"array","items":{"type":"string"}},"opts":{"type":"object"},` +
		`"size":{"type":"string","enum":["s","m","l"],"default":"m"},"n":{"type":"integer","minimum":1,"maximum":9},` +
		`"colors":{"type":"array","items":{"type":"string","enum":["red","green"]},"default":["green"]}},"required":["a","loud"]}}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}

	agents := do(h, http.MethodGet, "/ui/agents", "").Body.String()
	if !strings.Contains(agents, `<code title="Shown on top">label: string?</code>`) || !strings.Contains(agents, `<span class="muted">task</span>`) {
		t.Errorf("agents table missing input column:\n%s", agents)
	}

	form := do(h, http.MethodGet, "/ui/agents/adder/run", "").Body.String()
	for _, want := range []string{
		`name="field.a" type="number" step="1"`,
		`name="field.label" type="text"`,
		`name="field.loud" type="checkbox"`,
		`<textarea id="run-tags" name="field.tags"`,
		`<textarea id="run-opts" name="field.opts" rows="4" class="mono"`,
		`<small class="muted">Shown on top</small>`,
		`<option value="m" selected>m</option>`,
		`min="1" max="9"`,
		`name="field.colors" value="red" >`,
		`name="field.colors" value="green" checked>`,
	} {
		if !strings.Contains(form, want) {
			t.Errorf("adder form missing %q:\n%s", want, form)
		}
	}
	if form := do(h, http.MethodGet, "/ui/agents/nope/run", "").Body.String(); !strings.Contains(form, `class="failed"`) {
		t.Errorf("unknown agent form shows no error:\n%s", form)
	}

	post := func(path string, values url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(rec, req)
		return rec
	}
	if body := post("/ui/agents/echo/run", url.Values{"input": {"hi"}}).Body.String(); !strings.Contains(body, "agent not built") {
		t.Errorf("unbuilt run: %s", body)
	}
	buildAndWait(t, h, "/agents/echo/build")
	buildAndWait(t, h, "/agents/adder/build")

	body := post("/ui/agents/adder/run", url.Values{"field.a": {"x"}, "field.label": {"kept"}}).Body.String()
	if !strings.Contains(body, `field &#34;a&#34;: want an integer`) || !strings.Contains(body, `value="kept"`) {
		t.Errorf("bad input should re-render the form with the error and values:\n%s", body)
	}

	if body := post("/ui/agents/adder/run", url.Values{"field.a": {""}}).Body.String(); !strings.Contains(body, `missing property &#39;a&#39;`) {
		t.Errorf("blank required field should fail:\n%s", body)
	}

	stdin := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			d.mu.Lock()
			got := string(d.stdin)
			d.mu.Unlock()
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("stdin = %s, want %s", got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	rec := post("/ui/agents/adder/run", url.Values{
		"field.a": {" 7 "}, "field.label": {""}, "field.loud": {"true"},
		"field.tags": {"red\n\n blue \n"}, "field.opts": {`{"k": 1}`},
		"field.size": {"l"}, "field.colors": {"red", "green"},
	})
	if !strings.Contains(rec.Body.String(), "<h2>adder") || !strings.HasPrefix(rec.Header().Get("HX-Redirect"), "/#runs/") {
		t.Fatalf("adder run: HX-Redirect = %q\n%s", rec.Header().Get("HX-Redirect"), rec.Body)
	}
	stdin(`{"a":7,"colors":["red","green"],"loud":true,"opts":{"k":1},"size":"l","tags":["red","blue"]}`)

	rec = post("/ui/agents/echo/run", url.Values{"input": {"say hi"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h2>echo") || rec.Header().Get("HX-Trigger") != "refresh" {
		t.Fatalf("run: status = %d, HX-Trigger = %q\n%s", rec.Code, rec.Header().Get("HX-Trigger"), rec.Body)
	}
	stdin("say hi")
}

func TestUISecrets(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)
	post := func(values url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/ui/secrets", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(rec, req)
		return rec
	}
	conf := `{"modules":{"gh":{"secrets":{"github_token":{"required":true},"extra":{}}},"os":{}},` +
		`"agents":[{"name":"a","modules":["gh"]},{"name":"b","modules":["os"]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	t.Setenv("TREFOIL_SECRET_GITHUB_TOKEN", "")
	t.Setenv("TREFOIL_SECRET_EXTRA", "from-env")

	secrets := do(h, http.MethodGet, "/ui/secrets", "").Body.String()
	for _, want := range []string{"<code>GITHUB_TOKEN</code> <span class=\"muted\">required</span>", "pill failed\">missing", "pill built\" title=\"From TREFOIL_SECRET_EXTRA"} {
		if !strings.Contains(secrets, want) {
			t.Errorf("secrets fragment missing %q:\n%s", want, secrets)
		}
	}
	if agents := do(h, http.MethodGet, "/ui/agents", "").Body.String(); !strings.Contains(agents, "needs secret <code>github_token</code>") {
		t.Errorf("agents fragment missing needed secret:\n%s", agents)
	}

	rec := post(url.Values{"name": {"github_token"}, "value": {"ghp_ui"}})
	if body := rec.Body.String(); !strings.Contains(body, "Stored GITHUB_TOKEN") || !strings.Contains(body, "pill built\">stored") ||
		strings.Contains(body, "ghp_ui") || rec.Header().Get("HX-Trigger") != "refresh" {
		t.Fatalf("save secret: %s", body)
	}
	if agents := do(h, http.MethodGet, "/ui/agents", "").Body.String(); strings.Contains(agents, "needs secret") {
		t.Errorf("agents fragment still needs secret:\n%s", agents)
	}

	if body := post(url.Values{"name": {"not-env"}, "value": {"x"}}).Body.String(); !strings.Contains(body, "secret name must match") {
		t.Errorf("bad name: %s", body)
	}
	if body := post(url.Values{"name": {"ok"}, "value": {""}}).Body.String(); !strings.Contains(body, "value must not be empty") {
		t.Errorf("empty value: %s", body)
	}
}

func TestUIParams(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)
	conf := `{"params":{"type":"object","properties":{"repo":{"type":"string","description":"owner/name"},"limit":{"type":"integer","default":5}},"required":["repo"]},` +
		`"modules":{"os":{}},"defaults":{"modules":["os"]},` +
		`"agents":[{"name":"a","input_schema":{"type":"object","properties":{"repo":{"type":"string"},"limit":{"type":"integer"}},"required":["repo"]}}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	page := do(h, http.MethodGet, "/ui/params", "").Body.String()
	for _, want := range []string{`name="field.repo"`, "owner/name", `value="5"`, "pill failed"} {
		if !strings.Contains(page, want) {
			t.Errorf("params page missing %q:\n%s", want, page)
		}
	}

	post := func(form url.Values) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/ui/params", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if got := post(url.Values{"field.repo": {"acme/app"}, "field.limit": {"x"}}); !strings.Contains(got, "want an integer") || !strings.Contains(got, `value="acme/app"`) {
		t.Fatalf("bad save should keep the form:\n%s", got)
	}
	// The limit equals its default, so it stays unset.
	if got := post(url.Values{"field.repo": {"acme/app"}, "field.limit": {"5"}}); !strings.Contains(got, "Saved params") {
		t.Fatalf("save:\n%s", got)
	}
	var rows []paramRow
	_ = json.Unmarshal(do(h, http.MethodGet, "/params", "").Body.Bytes(), &rows)
	if rows[0].Value != "acme/app" || rows[0].Source != "set" || rows[1].Source != "default" {
		t.Fatalf("params after save = %+v", rows)
	}

	// The run form starts with the param's value.
	buildAndWait(t, h, "/agents/a/build")
	form := do(h, http.MethodGet, "/ui/agents/a/run", "").Body.String()
	if !strings.Contains(form, `value="acme/app"`) || !strings.Contains(form, ">param</span>") {
		t.Fatalf("run form missing param value:\n%s", form)
	}
}

func TestUIModules(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)
	if body := do(h, http.MethodGet, "/ui/modules", "").Body.String(); !strings.Contains(body, "no config uploaded") {
		t.Fatalf("modules before conf: %s", body)
	}
	conf := `{"modules":{"gh":{"secrets":{"github_token":{"required":true}},"mcps":[{"name":"github","url":"https://mcp.example/"}]},` +
		`"os":{"tools":[{"name":"Write","description":"Write a file","command":{"language":"python","code":"def Write(): pass"}}],"server_tools":[{"name":"web_search"}]},"idle":{}},` +
		`"defaults":{"modules":["os"]},"agents":[{"name":"a","modules":["gh"]},{"name":"b"}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	body := do(h, http.MethodGet, "/ui/modules", "").Body.String()
	for _, want := range []string{
		"<code>os</code>", "used by a, b", "<code>Write</code>", "python", "Write a file",
		"<code>web_search</code>", "<code>github</code>", "https://mcp.example/",
		"<code>github_token</code> <span class=\"muted\">required</span>", "<code>idle</code>", "unused",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("modules fragment missing %q:\n%s", want, body)
		}
	}
}

func TestUITimeline(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	d.logs = "[harness] agent=echo\n" +
		`x-trefoil-event: start {"model":"claude-x","tools":["Write"],"ts":1}` + "\n" +
		`x-trefoil-event: model {"turn":1,"stop_reason":"tool_use","input_tokens":10,"output_tokens":5,"text":"writing","ts":2}` + "\n" +
		`x-trefoil-event: tool_call {"turn":1,"id":"tu1","tool":"Write","input":{"path":"/workspace/out/a.md"},"ts":3}` + "\n" +
		`x-trefoil-event: tool_result {"turn":1,"id":"tu1","tool":"Write","is_error":true,"ms":42,"output":"Error: disk full","ts":4}` + "\n" +
		`x-trefoil-event: tool_call {"turn":1,"id":"tu2","tool":"Bash","input":{"cmd":"ls"},"ts":5}` + "\n" +
		`x-trefoil-event: done {"turns":2,"tokens":15,"cost_usd":0.0012,"ts":6}` + "\n"
	if rec := do(h, http.MethodPost, "/conf", `{"modules":{"os":{}},"defaults":{"modules":["os"]},"agents":[{"name":"echo"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/echo/build")
	var run map[string]any
	_ = json.Unmarshal(do(h, http.MethodPost, "/agents/echo/run", "hi").Body.Bytes(), &run)
	id, _ := run["session_id"].(string)

	var page string
	deadline := time.Now().Add(5 * time.Second)
	for page = do(h, http.MethodGet, "/ui/sessions/"+id, "").Body.String(); strings.Contains(page, `pill running">running</span></h2>`); page = do(h, http.MethodGet, "/ui/sessions/"+id, "").Body.String() {
		if time.Now().After(deadline) {
			t.Fatal("session still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{
		"Harness started", "claude-x · tools: Write",
		"Model · turn 1", "tool_use · 10 in / 5 out tokens", "writing",
		`<li class="ev ev-tool failed">`, "<strong>Write</strong>", `path=&#34;/workspace/out/a.md&#34;`, "42ms", "Error: disk full",
		"<strong>Bash</strong>", "no result",
		"Answered", "2 turns · 15 tokens · ≈$0.0012",
		"Run succeeded",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("run page missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "x-trefoil-event") {
		t.Errorf("logs should not show event lines:\n%s", page)
	}

	var events struct {
		Events []map[string]any `json:"events"`
	}
	rec := do(h, http.MethodGet, "/agents/echo/sessions/"+id+"/events", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil || len(events.Events) != 6 || events.Events[2]["type"] != "tool_call" {
		t.Errorf("events: status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestUsageChart(t *testing.T) {
	events := sessions.ParseEvents("x-trefoil-event: model {\"ts\":1}\n" +
		"x-trefoil-event: limit {\"ts\":2,\"tokens\":900,\"cost_usd\":0.5}\n")
	if tokens, cost, ok := runUsage(events); !ok || tokens != 900 || cost != 0.5 {
		t.Errorf("runUsage = %v, %v, %v", tokens, cost, ok)
	}
	if _, _, ok := runUsage(sessions.ParseEvents("x-trefoil-event: start {\"ts\":1}\n")); ok {
		t.Error("runUsage without done or limit: ok = true")
	}

	u := newUsageChart([]float64{100, 300}, []float64{0.25, 0.5})
	if u.Runs != 2 || u.Tokens != 400 || u.Cost != 0.75 {
		t.Errorf("chart totals = %+v", u)
	}
	if u.TokenLine != "0.0,18.3 120.0,1.0" {
		t.Errorf("TokenLine = %q", u.TokenLine)
	}
	if got := sparkline([]float64{0}); got != "0.0,27.0 120.0,27.0" {
		t.Errorf("sparkline of one zero = %q", got)
	}
}

func TestRunRow(t *testing.T) {
	model := "x-trefoil-event: model {\"ts\":1,\"input_tokens\":100,\"output_tokens\":20}\n"
	running := newRunRow(sessions.StatusInfo{}, sessions.ParseEvents(model+model))
	if running.Turns != 2 || running.Tokens != 240 || running.Finished || running.Line != "0.0,1.0 120.0,1.0" {
		t.Errorf("running row = %+v", running)
	}
	done := newRunRow(sessions.StatusInfo{}, sessions.ParseEvents(model+"x-trefoil-event: done {\"ts\":2,\"tokens\":130,\"cost_usd\":0.01}\n"))
	if done.Turns != 1 || done.Tokens != 130 || done.Cost != 0.01 || !done.Finished {
		t.Errorf("done row = %+v", done)
	}
}

func TestBudgets(t *testing.T) {
	start := time.Now()
	end := start.Add(50 * time.Second)
	info := sessions.StatusInfo{Status: sessions.Succeeded, CreatedAt: start, FinishedAt: &end,
		Limits: conf.Limits{MaxTurns: 4, TokenBudget: 1000, MaxCostUSD: 1, TimeoutSeconds: 100}}
	old := "x-trefoil-event: model {\"ts\":1,\"input_tokens\":100,\"output_tokens\":20}\n"
	totals := "x-trefoil-event: model {\"ts\":2,\"input_tokens\":100,\"output_tokens\":20,\"tokens\":900,\"cost_usd\":0.5}\n"

	got := budgets(info, sessions.ParseEvents(old+totals), "")
	want := []budgetRow{
		{Name: "Turns", Used: "2", Limit: "4", Pct: 50, Level: "finished"},
		{Name: "Tokens", Used: "900", Limit: "1000", Pct: 90, Level: "finished"},
		{Name: "Cost", Used: "≈$0.5000", Limit: "$1.00", Pct: 50, Level: "finished"},
		{Name: "Time", Used: "50s", Limit: "1m40s", Pct: 50, Level: "finished"},
	}
	if len(got) != len(want) {
		t.Fatalf("budgets = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("budgets[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Without running totals, tokens are summed per turn and cost is unknown.
	info.Status = sessions.Running
	info.Limits = conf.Limits{TokenBudget: 200, MaxCostUSD: 1}
	got = budgets(info, sessions.ParseEvents(old+old+"x-trefoil-event: limit {\"ts\":3,\"reason\":\"token_budget\"}\n"), "")
	if got[0].Used != "240" || got[0].Pct != 100 || got[0].Level != "over" || got[1].Used != "?" || got[1].Level != "running" {
		t.Errorf("budgets without totals = %+v", got)
	}

	// Older runs have no stored limits; they come from the harness's log
	// line. Cost shows without a bar when it has no limit.
	info.Limits = conf.Limits{}
	logs := "[harness] agent=pdf model=m limits={'max_turns': 8, 'timeout_seconds': 300}\n"
	got = budgets(info, nil, logs)
	if len(got) != 3 || got[0].Limit != "8" || got[1].Name != "Cost" || got[1].Limit != "" || got[2].Limit != "5m0s" {
		t.Errorf("budgets from logs = %+v", got)
	}

	// Without events, usage comes from the harness's closing line.
	got = budgets(info, nil, logs+"[harness] done in 3 turns; tokens=49 cost≈$0.0002\n")
	if got[0].Used != "3" || got[1].Used != "≈$0.0002" {
		t.Errorf("budgets from logged usage = %+v", got)
	}
}

func TestUIAgent(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)
	conf := `{"modules":{"os":{"tools":[{"name":"Write","description":"Write a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}},"command":{"language":"python","code":"def Write(): pass"}}],` +
		`"server_tools":[{"name":"web_search"}],"mcps":[{"name":"github","url":"https://mcp.example/"}],` +
		`"skills":[{"source":{"SKILL.md":"---\nname: notes\ndescription: Write release notes\n---\nbody"}}]}},` +
		`"defaults":{"modules":["os"],"model":"claude-x"},"agents":[{"name":"echo","prompts":[{"source":"You echo <things>.","role":"system"},{"source":"Be brief.","role":"user"}]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	body := do(h, http.MethodGet, "/ui/agents/echo", "").Body.String()
	for _, want := range []string{
		"<h2>echo</h2>", "<code>claude-x</code>", "You echo &lt;things&gt;.", "<h3>user</h3>", "Be brief.",
		"<code>Write</code>", "Write a file", "&#34;path&#34;", "<code>web_search</code>", "https://mcp.example/",
		"<code>Skill</code>", "<code>notes</code>", "Write release notes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agent page missing %q:\n%s", want, body)
		}
	}
	if body := do(h, http.MethodGet, "/ui/agents/nope", "").Body.String(); !strings.Contains(body, `class="error"`) {
		t.Errorf("unknown agent: %s", body)
	}
	if body := do(h, http.MethodGet, "/ui/agents", "").Body.String(); !strings.Contains(body, `href="#agents/echo"`) {
		t.Errorf("agents table has no link to the agent page:\n%s", body)
	}
}

func TestCallInput(t *testing.T) {
	input := map[string]any{
		"command": "export PATH=\"/shims:$PATH\"\ncd /w && npm test 2>&1; echo \"EXIT:$?\"",
		"timeout": 60.0,
		"opts":    map[string]any{"a": "<b>"},
	}
	want := "command:\n  export PATH=\"/shims:$PATH\"\n  cd /w && npm test 2>&1; echo \"EXIT:$?\"\n" +
		"opts:\n  {\n    \"a\": \"<b>\"\n  }\ntimeout: 60"
	if got := callInput(input); got != want {
		t.Errorf("callInput =\n%s\nwant\n%s", got, want)
	}
	if got, want := callSummary(input), `command="export PATH="/shims:$PATH" cd /w && npm test 2>&1; echo …, opts={"a":"<b>"}, timeout=60`; got != want {
		t.Errorf("callSummary = %s, want %s", got, want)
	}
}
