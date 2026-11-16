package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"trefoil/api/internal/bricks"
	"trefoil/api/internal/build"
	"trefoil/api/internal/conf"
	"trefoil/api/internal/jobs"
	"trefoil/api/internal/schema"
	"trefoil/api/internal/sessions"
	"trefoil/api/internal/store"
)

type fakeDocker struct {
	mu      sync.Mutex
	built   map[string]*build.Context
	stdin   []byte
	runOpts build.RunOptions
	stdout  string // what Run prints; defaults to "ran <tag>"
	logs    string // extra stderr Run writes after its first line
	// artifacts are files Run leaves in opts.Artifacts, by relative path.
	artifacts map[string]string
	// hold makes Run wait for its context to end, like a run being stopped.
	hold bool
}

func (f *fakeDocker) Build(ctx context.Context, bc *build.Context, dir, tag string, logs io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.built[tag] = bc
	_, _ = io.WriteString(logs, "built "+tag)
	return nil
}

func (f *fakeDocker) Run(ctx context.Context, tag string, stdin []byte, opts build.RunOptions) (*build.RunResult, error) {
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()
	if hold {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.built[tag] == nil {
		return nil, build.ErrImageNotFound
	}
	f.stdin, f.runOpts = stdin, opts
	if opts.Logs != nil {
		_, _ = io.WriteString(opts.Logs, "[harness] running "+tag+"\n"+f.logs)
	}
	for name, data := range f.artifacts {
		full := filepath.Join(opts.Artifacts, name)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(data), 0o644)
	}
	stdout := f.stdout
	if stdout == "" {
		stdout = "ran " + tag
	}
	return &build.RunResult{Stdout: stdout}, nil
}

func (f *fakeDocker) ImageExists(ctx context.Context, tag string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.built[tag] != nil, nil
}

// buildAndWait starts a build and polls its job until it finishes.
func buildAndWait(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	rec := do(h, http.MethodPost, path, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body)
	}
	var job map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	for i := 0; i < 200; i++ {
		rec = do(h, http.MethodGet, "/jobs/"+job["id"].(string), "")
		_ = json.Unmarshal(rec.Body.Bytes(), &job)
		if job["status"] == "succeeded" || job["status"] == "failed" {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %v did not finish", job["id"])
	return nil
}

// runAndWait starts a run, waits for its session to finish and returns the
// session id and its result.
func runAndWait(t *testing.T, h http.Handler, agent, body string) (string, map[string]any) {
	t.Helper()
	rec := do(h, http.MethodPost, "/agents/"+agent+"/run", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: status = %d, body = %s", rec.Code, rec.Body)
	}
	var started map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	id := started["session_id"].(string)
	base := "/agents/" + agent + "/sessions/" + id
	for i := 0; i < 200; i++ {
		var status map[string]any
		_ = json.Unmarshal(do(h, http.MethodGet, base+"/status", "").Body.Bytes(), &status)
		if status["status"] != "running" {
			rec = do(h, http.MethodGet, base+"/result", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("result: status = %d, body = %s", rec.Code, rec.Body)
			}
			var result map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &result)
			return id, result
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s did not finish", id)
	return "", nil
}

func newTestHandler(t *testing.T) http.Handler {
	h, _ := newTestHandlerWithDocker(t)
	return h
}

func newTestHandlerWithDocker(t *testing.T) (http.Handler, *fakeDocker) {
	t.Helper()
	v, err := schema.NewConf()
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDocker{built: map[string]*build.Context{}}
	dataDir := t.TempDir()
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)), store.NewLocal(dataDir), v, d, filepath.Join(dataDir, "builds"), bricks.NewLibrary(filepath.Join(dataDir, "shared"))), d
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestHealth(t *testing.T) {
	rec := do(newTestHandler(t), http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestConfRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	conf := `{"bricks":{"os":{"tools":[{"name":"Write","command":{"language":"python","code":"file://commands/write.py"}}]}},"defaults":{"bricks":["os"],"max_tokens":1000}}`

	if rec := do(h, http.MethodGet, "/conf", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("GET before POST: status = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec := do(h, http.MethodGet, "/conf", "")
	if rec.Code != http.StatusOK || rec.Body.String() != conf {
		t.Fatalf("GET: status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestConfWithoutBricks(t *testing.T) {
	h := newTestHandler(t)
	if rec := do(h, http.MethodPost, "/conf", `{"agents":[{"name":"echo"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("POST: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec := do(h, http.MethodGet, "/agents", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"bricks":[]`) {
		t.Fatalf("GET /agents: status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestConfRejectsInvalid(t *testing.T) {
	h := newTestHandler(t)
	cases := map[string]string{
		"malformed":       `{"bricks":`,
		"wrong type":      `{"bricks":{},"defaults":{"max_tokens":"lots"}}`,
		"unknown top key": `{"bricks":{},"extra":1}`,
		"agent no name":   `{"bricks":{},"agents":[{"bricks":[]}]}`,
		"duplicate agent": `{"bricks":{},"agents":[{"name":"a"},{"name":"a"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := do(h, http.MethodPost, "/conf", body); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422, body = %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestRunValidatesInput(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	conf := `{"agents":[{"name":"echo","input_schema":{"type":"object","properties":{"statement":{"type":"string"}},"required":["statement"],"additionalProperties":false},"outputs":[{"echo":"string"}]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/echo/build")

	for _, body := range []string{`{"task":"hi"}`, `{"statement":1}`, `not json`} {
		if rec := do(h, http.MethodPost, "/agents/echo/run", body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("run %s: status = %d, want 422, body = %s", body, rec.Code, rec.Body)
		}
	}
	d.stdout = `{"echo":"hi"}`
	if _, res := runAndWait(t, h, "echo", `{"statement":"hi"}`); res["output"].(map[string]any)["echo"] != "hi" {
		t.Fatalf("result = %v, want parsed output", res)
	}
	if string(d.stdin) != `{"statement":"hi"}` {
		t.Fatalf("stdin = %s", d.stdin)
	}

	var spec map[string]any
	if err := json.Unmarshal(d.built["trefoil-agent-echo:latest"].Files["agent.json"], &spec); err != nil {
		t.Fatal(err)
	}
	if spec["input_schema"] == nil || spec["output_schema"] == nil {
		t.Fatalf("agent.json missing schemas: %v", spec)
	}
}

func TestRunFillsParams(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	conf := `{"params":{"type":"object","properties":{"repo":{"type":"string"},"limit":{"type":"integer","default":5},"unused":{"type":"string"}},"required":["repo"]},` +
		`"agents":[{"name":"a","input_schema":{"type":"object","properties":{"repo":{"type":"string"},"limit":{"type":"integer"},"version":{"type":"string"}},"required":["repo"]}}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/a/build")

	// A required param with no value rejects runs that need it.
	rec := do(h, http.MethodPost, "/agents/a/run", `{"version":"v1"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"missing_params":["repo"]`) {
		t.Fatalf("run without param: status = %d, body = %s", rec.Code, rec.Body)
	}

	for path, body := range map[string]string{
		"/param/repo":  `{"value":"acme/app"}`,
		"/param/limit": `{"value":"10"}`, // parsed by the param's type
	} {
		if rec := do(h, http.MethodPost, path, body); rec.Code != http.StatusCreated {
			t.Fatalf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body)
		}
	}
	for path, body := range map[string]string{
		"/param/nope":  `{"value":"x"}`,
		"/param/limit": `{"value":"ten"}`,
		"/param/repo":  `{"value":3}`,
	} {
		if rec := do(h, http.MethodPost, path, body); rec.Code < 400 {
			t.Errorf("POST %s %s: status = %d, want an error", path, body, rec.Code)
		}
	}

	runAndWait(t, h, "a", `{"version":"v1"}`)
	if got, want := string(d.stdin), `{"limit":10,"repo":"acme/app","version":"v1"}`; got != want {
		t.Fatalf("stdin = %s, want %s", got, want)
	}
	runAndWait(t, h, "a", `{"repo":"other/app","limit":1}`)
	if got, want := string(d.stdin), `{"repo":"other/app","limit":1}`; got != want {
		t.Fatalf("stdin = %s, want input to win over params", got)
	}

	// Unset falls back to the default.
	if rec := do(h, http.MethodDelete, "/param/limit", ""); rec.Code != http.StatusOK {
		t.Fatalf("DELETE /param/limit: status = %d, body = %s", rec.Code, rec.Body)
	}
	runAndWait(t, h, "a", `{}`)
	if got, want := string(d.stdin), `{"limit":5,"repo":"acme/app"}`; got != want {
		t.Fatalf("stdin = %s, want %s", got, want)
	}

	var rows []paramRow
	_ = json.Unmarshal(do(h, http.MethodGet, "/params", "").Body.Bytes(), &rows)
	if len(rows) != 3 || rows[0].Source != "set" || rows[1].Source != "default" || rows[2].Source != "" ||
		!rows[0].Required || len(rows[0].Agents) != 1 || len(rows[2].Agents) != 0 {
		t.Fatalf("GET /params = %+v", rows)
	}
}

func TestConfRejectsBadFields(t *testing.T) {
	h := newTestHandler(t)
	for _, body := range []string{
		`{"agents":[{"name":"a","input_schema":{"type":"object","properties":{"x":{"type":"text"}}}}]}`,
		`{"agents":[{"name":"a","input_schema":{"type":"string"}}]}`,
		`{"agents":[{"name":"a","outputs":[{"x":"object"}]}]}`,
		`{"agents":[{"name":"a","input_schema":"string"}]}`,
	} {
		if rec := do(h, http.MethodPost, "/conf", body); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status = %d, want 422", body, rec.Code)
		}
	}
}

func TestAgentBuildAndRun(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)

	if rec := do(h, http.MethodPost, "/agents/web/build", ""); rec.Code != http.StatusConflict {
		t.Fatalf("build before conf: status = %d, want 409", rec.Code)
	}

	conf := `{"bricks":{"os":{"layers":[{"title":"t","script":"echo hi"}]}},"agents":[{"name":"web","bricks":["os"]},{"name":"fixer"}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}

	if rec := do(h, http.MethodPost, "/agents/fixer/run", ""); rec.Code != http.StatusConflict {
		t.Fatalf("run before build: status = %d, want 409", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/agents/missing/build", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status = %d, want 404", rec.Code)
	}

	job := buildAndWait(t, h, "/agents/web/build")
	if job["status"] != "succeeded" || job["image"] != "trefoil-agent-web:latest" ||
		!strings.HasSuffix(job["logs"].(string), "built trefoil-agent-web:latest") ||
		!strings.HasSuffix(job["context_dir"].(string), filepath.Join("builds", "web", job["id"].(string))) || !strings.Contains(job["dockerfile"].(string), "RUN echo hi") {
		t.Fatalf("job = %v", job)
	}

	buildAndWait(t, h, "/agents/build/fixer")
	if rec := do(h, http.MethodGet, "/jobs/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: status = %d, want 404", rec.Code)
	}
	if d.built["trefoil-agent-fixer:latest"] == nil {
		t.Fatalf("alias did not build fixer image; built = %v", d.built)
	}

	id, res := runAndWait(t, h, "fixer", `{"task":"x"}`)
	if res["status"] != "succeeded" || res["stdout"] != "ran trefoil-agent-fixer:latest" || res["output"] != nil {
		t.Fatalf("result = %v", res)
	}
	rec := do(h, http.MethodGet, "/agents/fixer/sessions/"+id+"/logs?offset=10", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"logs":"running trefoil-agent-fixer:latest\n"`) {
		t.Fatalf("logs: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec = do(h, http.MethodGet, "/agents/fixer/sessions/"+id+"/logs?download=1", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "[harness] running trefoil-agent-fixer:latest\n" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="fixer-`+id+`.log"` {
		t.Fatalf("download logs: status = %d, headers = %v, body = %q", rec.Code, rec.Header(), rec.Body)
	}
	if rec := do(h, http.MethodGet, "/agents/web/sessions/"+id+"/status", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("session under wrong agent: status = %d, want 404", rec.Code)
	}
	if string(d.stdin) != `{"task":"x"}` {
		t.Fatalf("stdin = %q", d.stdin)
	}
}

func TestListAgents(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)

	if rec := do(h, http.MethodGet, "/agents", ""); rec.Code != http.StatusConflict {
		t.Fatalf("list before conf: status = %d, want 409", rec.Code)
	}

	conf := `{"bricks":{"os":{},"web":{}},"defaults":{"bricks":["os"],"model":"claude-sonnet-5","provider":"bedrock"},"agents":[{"name":"web","bricks":["web","os"]},{"name":"fixer","prompts":[{"source":"p"}]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/fixer/build")

	rec := do(h, http.MethodGet, "/agents", "")
	want := `[{"name":"web","model":"claude-sonnet-5","provider":"bedrock","bricks":["os","web"],"prompts":0,"image":"trefoil-agent-web:latest","built":false},` +
		`{"name":"fixer","model":"claude-sonnet-5","provider":"bedrock","bricks":["os"],"prompts":1,"image":"trefoil-agent-fixer:latest","built":true}]` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("GET /agents: status = %d\n got: %s\nwant: %s", rec.Code, rec.Body, want)
	}
}

func TestListJobs(t *testing.T) {
	h, _ := newTestHandlerWithDocker(t)
	conf := `{"bricks":{},"agents":[{"name":"a"},{"name":"b"}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodGet, "/jobs", ""); rec.Body.String() != "[]\n" {
		t.Fatalf("empty list = %s", rec.Body)
	}
	buildAndWait(t, h, "/agents/a/build")
	buildAndWait(t, h, "/agents/b/build")

	list := func(query string) []map[string]any {
		var out []map[string]any
		_ = json.Unmarshal(do(h, http.MethodGet, "/jobs"+query, "").Body.Bytes(), &out)
		return out
	}
	all := list("")
	if len(all) != 2 || all[0]["agent"] != "b" || all[0]["logs"] != "" {
		t.Fatalf("all jobs = %v", all)
	}
	if got := list("?agent=a"); len(got) != 1 || got[0]["agent"] != "a" {
		t.Fatalf("agent filter = %v", got)
	}
	if got := list("?status=failed"); len(got) != 0 {
		t.Fatalf("status filter = %v", got)
	}
}

func TestPostConfWritesBuildContexts(t *testing.T) {
	v, err := schema.NewConf()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store.NewLocal(dataDir), v,
		&fakeDocker{built: map[string]*build.Context{}}, filepath.Join(dataDir, "builds"), bricks.NewLibrary(filepath.Join(dataDir, "shared")))

	conf := `{"bricks":{"os":{"layers":[{"title":"t","script":"echo hi"}]}},"agents":[{"name":"fixer","bricks":["os"]}]}`
	rec := do(h, http.MethodPost, "/conf", conf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"agents/fixer/Dockerfile"`) {
		t.Fatalf("response = %s", rec.Body)
	}
	df, err := os.ReadFile(filepath.Join(dataDir, "agents", "fixer", "Dockerfile"))
	if err != nil || !strings.Contains(string(df), "RUN echo hi") || !strings.Contains(string(df), "/trefoil/harness/main.py") {
		t.Fatalf("Dockerfile = %q, err = %v", df, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "agents", "fixer", "harness", "main.py")); err != nil {
		t.Fatal(err)
	}

	bad := `{"bricks":{},"agents":[{"name":"x","bricks":["missing"]}]}`
	if rec := do(h, http.MethodPost, "/conf", bad); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown brick: status = %d, want 422", rec.Code)
	}
}

func TestSharedBricks(t *testing.T) {
	v, err := schema.NewConf()
	if err != nil {
		t.Fatal(err)
	}
	dataDir, shared := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(shared, "core", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "core", "web", "config.yml"), []byte("layers:\n  - title: web\n    script: echo shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store.NewLocal(dataDir), v,
		&fakeDocker{built: map[string]*build.Context{}}, filepath.Join(dataDir, "builds"), bricks.NewLibrary(shared))

	conf := `{"bricks":{},"agents":[{"name":"a","bricks":["core:web"]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	df, _ := os.ReadFile(filepath.Join(dataDir, "agents", "a", "Dockerfile"))
	if !strings.Contains(string(df), "# core:web: web\nRUN echo shared") {
		t.Fatalf("Dockerfile = %s", df)
	}
	if rec := do(h, http.MethodGet, "/agents", ""); !strings.Contains(rec.Body.String(), `"bricks":["core:web"]`) {
		t.Fatalf("GET /agents = %s", rec.Body)
	}

	for name, body := range map[string]string{
		"missing shared brick": `{"bricks":{},"agents":[{"name":"a","bricks":["core:nope"]}]}`,
		"shadowed locally":     `{"bricks":{"core:web":{}},"agents":[{"name":"a","bricks":["core:web"]}]}`,
	} {
		if rec := do(h, http.MethodPost, "/conf", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422, body = %s", name, rec.Code, rec.Body)
		}
	}
}

func TestRunPassesLimits(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	conf := `{"bricks":{},"defaults":{"limits":{"max_turns":10,"timeout_seconds":30,"max_cost_usd":1}},` +
		`"agents":[{"name":"a","limits":{"max_turns":3,"token_budget":50000}}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/a/build")
	runAndWait(t, h, "a", "go")
	if got, want := d.runOpts.Env["TREFOIL_LIMITS"], `{"max_turns":3,"token_budget":50000,"timeout_seconds":30,"max_cost_usd":1}`; got != want {
		t.Fatalf("TREFOIL_LIMITS = %s, want %s", got, want)
	}
	if d.runOpts.Timeout != 30*time.Second {
		t.Fatalf("timeout = %s", d.runOpts.Timeout)
	}
	bad := `{"bricks":{},"agents":[{"name":"a","limits":{"max_turns":0}}]}`
	if rec := do(h, http.MethodPost, "/conf", bad); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid limits: status = %d, want 422", rec.Code)
	}
}

func TestStopRun(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	if rec := do(h, http.MethodPost, "/conf", `{"bricks":{},"agents":[{"name":"a"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/a/build")
	d.mu.Lock()
	d.hold = true
	d.mu.Unlock()
	rec := do(h, http.MethodPost, "/agents/a/run", "go")
	var started map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	base := "/agents/a/sessions/" + started["session_id"].(string)

	if rec := do(h, http.MethodPost, "/agents/b/sessions/"+started["session_id"].(string)+"/stop", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stop under another agent: status = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodPost, base+"/stop", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body)
	}
	var status map[string]any
	for i := 0; i < 200; i++ {
		_ = json.Unmarshal(do(h, http.MethodGet, base+"/status", "").Body.Bytes(), &status)
		if status["status"] != "running" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status["status"] != "failed" || status["error"] != "stopped" {
		t.Fatalf("status after stop = %v", status)
	}
	if rec := do(h, http.MethodPost, base+"/stop", ""); rec.Code != http.StatusConflict {
		t.Fatalf("second stop: status = %d, want 409", rec.Code)
	}
}

func TestRunPassesSecrets(t *testing.T) {
	h, d := newTestHandlerWithDocker(t)
	conf := `{"bricks":{"gh":{"secrets":{"github_token":{"required":true},"extra":{}}}},` +
		`"agents":[{"name":"a","bricks":["gh"]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/a/build")

	t.Setenv("TREFOIL_SECRET_GITHUB_TOKEN", "")
	rec := do(h, http.MethodPost, "/agents/a/run", "go")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"missing_secrets":["github_token"]`) {
		t.Fatalf("missing secret: status = %d, body = %s", rec.Code, rec.Body)
	}
	if d.stdin != nil {
		t.Fatalf("run started without required secret")
	}

	// A stored secret satisfies the run and wins over the environment.
	if rec := do(h, http.MethodPost, "/secret/github_token", `{"value":"ghp_stored"}`); rec.Code != http.StatusCreated ||
		strings.Contains(rec.Body.String(), "ghp_stored") {
		t.Fatalf("POST /secret: status = %d, body = %s", rec.Code, rec.Body)
	}
	t.Setenv("TREFOIL_SECRET_GITHUB_TOKEN", "ghp_env")
	runAndWait(t, h, "a", "go")
	if got := d.runOpts.Secrets["GITHUB_TOKEN"]; got != "ghp_stored" {
		t.Fatalf("GITHUB_TOKEN = %q, want stored value", got)
	}

	for _, tc := range []struct{ path, body string }{
		{"/secret/not-env", `{"value":"x"}`},
		{"/secret/ok", `{"value":""}`},
		{"/secret/ok", `plain`},
	} {
		if rec := do(h, http.MethodPost, tc.path, tc.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %s %s: status = %d, want 400", tc.path, tc.body, rec.Code)
		}
	}

	bad := `{"bricks":{"gh":{"secrets":{"not-env":{}}}},"agents":[{"name":"a","bricks":["gh"]}]}`
	if rec := do(h, http.MethodPost, "/conf", bad); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid secret name: status = %d, want 422", rec.Code)
	}
}

func TestApplyProvider(t *testing.T) {
	s := &server{providers: []string{"bedrock"}}

	c := &conf.Conf{}
	if err := s.applyProvider(c); err != nil || c.Defaults.Provider != "bedrock" {
		t.Fatalf("default: provider=%q err=%v", c.Defaults.Provider, err)
	}
	c = &conf.Conf{Defaults: conf.Defaults{Provider: "anthropic"}}
	if err := s.applyProvider(c); err == nil {
		t.Fatal("want error for disabled provider")
	}
	if err := (&server{}).applyProvider(c); err != nil {
		t.Fatalf("no allowlist: %v", err)
	}
}

func TestRunArtifacts(t *testing.T) {
	v, err := schema.NewConf()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	sess, err := sessions.Open(filepath.Join(dataDir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDocker{built: map[string]*build.Context{}, artifacts: map[string]string{"page.pdf": "%PDF-1.4", "notes/a.html": "<script>x</script>"}}
	h := New(slog.New(slog.NewTextHandler(io.Discard, nil)), store.NewLocal(dataDir), v, d, filepath.Join(dataDir, "builds"),
		bricks.NewLibrary(filepath.Join(dataDir, "shared")), WithManagers(jobs.NewManager(), sess))

	if rec := do(h, http.MethodPost, "/conf", `{"agents":[{"name":"pdf"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	buildAndWait(t, h, "/agents/pdf/build")
	id, result := runAndWait(t, h, "pdf", "print it")
	if !strings.HasSuffix(d.runOpts.Artifacts, filepath.Join("sessions", id, "artifacts")) {
		t.Errorf("Artifacts = %q", d.runOpts.Artifacts)
	}
	want := []any{map[string]any{"path": "notes/a.html", "size": 18.0}, map[string]any{"path": "page.pdf", "size": 8.0}}
	if !reflect.DeepEqual(result["artifacts"], want) {
		t.Errorf("result artifacts = %v", result["artifacts"])
	}

	base := "/agents/pdf/sessions/" + id + "/artifacts"
	var list map[string]any
	_ = json.Unmarshal(do(h, http.MethodGet, base, "").Body.Bytes(), &list)
	if !reflect.DeepEqual(list["artifacts"], want) {
		t.Errorf("list = %v", list)
	}
	rec := do(h, http.MethodGet, base+"/page.pdf", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "%PDF-1.4" || rec.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("page.pdf: status = %d, type = %q, body = %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if rec := do(h, http.MethodGet, base+"/notes/a.html", ""); rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("html artifact served without sandbox: %v", rec.Header())
	}
	for _, p := range []string{base + "/missing.pdf", base + "/..%2f" + id + ".json", "/agents/other/sessions/" + id + "/artifacts"} {
		if rec := do(h, http.MethodGet, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", p, rec.Code)
		}
	}
}

func TestAgentContextZip(t *testing.T) {
	h := newTestHandler(t)
	if rec := do(h, http.MethodGet, "/agents/web/context", ""); rec.Code != http.StatusConflict {
		t.Fatalf("context before conf: status = %d, want 409", rec.Code)
	}
	conf := `{"bricks":{"os":{"layers":[{"title":"t","script":"echo hi"}]}},"agents":[{"name":"Web.App","bricks":["os"]}]}`
	if rec := do(h, http.MethodPost, "/conf", conf); rec.Code != http.StatusCreated {
		t.Fatalf("POST /conf: status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodGet, "/agents/missing/context", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status = %d, want 404", rec.Code)
	}

	rec := do(h, http.MethodGet, "/agents/Web.App/context", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="web.app-context.zip"` {
		t.Fatalf("status = %d, headers = %v, body = %s", rec.Code, rec.Header(), rec.Body)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(data)
	}
	if !strings.Contains(files["web.app/Dockerfile"], "RUN echo hi") || files["web.app/agent.json"] == "" {
		t.Fatalf("zip files = %v", slices.Collect(maps.Keys(files)))
	}
}
