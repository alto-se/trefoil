package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"trefoil/api/internal/build"
	"trefoil/api/internal/conf"
	"trefoil/api/internal/jobs"
	"trefoil/api/internal/sessions"
	"trefoil/api/internal/store"
)

const (
	maxRunInputBytes = 1 << 20
	buildTimeout     = 30 * time.Minute
)

// handleAgentAction routes POST /agents/{a}/{b}. The canonical forms
// /agents/{id}/build and /agents/{id}/run take precedence, so an agent
// named "build" is reached by /agents/build/build or /agents/build/run.
func (s *server) handleAgentAction(w http.ResponseWriter, r *http.Request) {
	a, b := r.PathValue("a"), r.PathValue("b")
	switch {
	case b == "build":
		s.buildAgent(w, r, a)
	case b == "run":
		s.runAgent(w, r, a)
	case a == "build":
		s.buildAgent(w, r, b)
	default:
		http.NotFound(w, r)
	}
}

type agentSummary struct {
	Name string `json:"name"`
	// Model and provider come from defaults; an empty model means the
	// harness default.
	Model       string            `json:"model,omitempty"`
	Provider    string            `json:"provider,omitempty"`
	Modules     []string          `json:"modules"`
	Prompts     int               `json:"prompts"`
	InputSchema *conf.InputSchema `json:"input_schema,omitempty"`
	Outputs     conf.Fields       `json:"outputs,omitempty"`
	Image       string            `json:"image"`
	Built       bool              `json:"built"`
	// MissingSecrets are required secrets with no value yet; runs are
	// rejected until each is set.
	MissingSecrets []string `json:"missing_secrets,omitempty"`
}

// handleListAgents lists the agents in the stored config and whether each
// one's image has been built.
func (s *server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.agentSummaries(r.Context(), c))
}

func (s *server) agentSummaries(ctx context.Context, c *conf.Conf) []agentSummary {
	agents := []agentSummary{}
	for i := range c.Agents {
		a := &c.Agents[i]
		tag := build.ImageTag(a.Name)
		built, err := s.docker.ImageExists(ctx, tag)
		if err != nil {
			s.logger.Warn("check image", "image", tag, "err", err)
		}
		// Other read errors are reported when a run reads the secrets.
		var missing missingSecretsError
		_, err = s.secretEnv(ctx, build.AgentSecrets(c, a))
		errors.As(err, &missing)
		agents = append(agents, agentSummary{
			Name:        a.Name,
			Model:       c.Defaults.Model,
			Provider:    c.Defaults.Provider,
			Modules:     build.AgentModules(c, a),
			Prompts:     len(a.Prompts),
			InputSchema: a.InputSchema,
			Outputs:     a.Outputs,
			Image:       tag,
			Built:       built,

			MissingSecrets: missing,
		})
	}
	return agents
}

func (s *server) buildAgent(w http.ResponseWriter, r *http.Request, name string) {
	job, status, err := s.startBuild(r.Context(), name)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job.Snapshot())
}

// handleJobRerun starts a new build of the job's agent. Like a fresh build,
// it uses the current config, so changes synced since the job are picked up.
func (s *server) handleJobRerun(w http.ResponseWriter, r *http.Request) {
	old, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	job, status, err := s.rerunJob(r.Context(), old)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job.Snapshot())
}

// rerunJob builds old's agent again. On error it also returns the HTTP
// status that fits it.
func (s *server) rerunJob(ctx context.Context, old *jobs.Job) (*jobs.Job, int, error) {
	snap := old.Snapshot()
	if snap.Kind != "build" {
		return nil, http.StatusConflict, fmt.Errorf("can't rerun a %q job", snap.Kind)
	}
	if snap.Status == jobs.Queued || snap.Status == jobs.Running {
		return nil, http.StatusConflict, errors.New("job is still " + string(snap.Status))
	}
	return s.startBuild(ctx, snap.Agent)
}

// startBuild composes the agent's build context from the stored config and
// builds its image in a background job. On error it also returns the HTTP
// status that fits it.
func (s *server) startBuild(ctx context.Context, name string) (*jobs.Job, int, error) {
	c, status, err := s.readConf(ctx)
	if err != nil {
		return nil, status, err
	}
	agent, err := c.Agent(name)
	if err != nil {
		return nil, http.StatusNotFound, err
	}
	bc, err := build.Compose(c, agent)
	if err != nil {
		return nil, http.StatusUnprocessableEntity, err
	}

	// The build outlives the request, so it gets its own context.
	tag := build.ImageTag(name)
	job := s.jobs.Start("build", name, tag, bc.Dockerfile, func(j *jobs.Job) error {
		ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
		defer cancel()
		dir := filepath.Join(s.buildDir, build.SafeName(name), j.ID())
		j.SetContextDir(dir)
		fmt.Fprintf(j, "build context: %s\n", dir)
		err := s.docker.Build(ctx, bc, dir, tag, j)
		if err != nil {
			s.logger.Error("build agent", "agent", name, "job", j.ID(), "err", err)
		} else {
			s.logger.Info("built agent", "agent", name, "job", j.ID(), "image", tag)
		}
		return err
	})
	return job, 0, nil
}

// handleAgentContext sends the agent's build context (the Dockerfile plus
// every file it copies) as <agent>-context.zip, unpacking to a folder that
// `docker build` builds the same image from.
func (s *server) handleAgentContext(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	agent, err := c.Agent(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	bc, err := build.Compose(c, agent)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var buf bytes.Buffer
	dir := build.SafeName(name)
	if err := bc.WriteZip(&buf, dir); err != nil {
		s.logger.Error("zip build context", "agent", name, "err", err)
		writeError(w, http.StatusInternalServerError, "failed to zip build context")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-context.zip"`, dir))
	_, _ = w.Write(buf.Bytes())
}

func (s *server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job.Snapshot())
}

// handleListJobs lists jobs newest first, without logs or Dockerfiles.
// Optional filters: ?agent=<name>&status=<queued|running|succeeded|failed>.
func (s *server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	agent, status := r.URL.Query().Get("agent"), r.URL.Query().Get("status")
	out := []jobs.Snapshot{}
	for _, j := range s.jobs.List() {
		snap := j.Snapshot()
		if (agent != "" && snap.Agent != agent) || (status != "" && string(snap.Status) != status) {
			continue
		}
		snap.Logs, snap.Dockerfile = "", ""
		out = append(out, snap)
	}
	writeJSON(w, http.StatusOK, out)
}

// runAgent starts a session: the agent's image runs in the background with
// the request body on stdin. Follow it under /agents/{name}/sessions/{id}.
func (s *server) runAgent(w http.ResponseWriter, r *http.Request, name string) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	agent, err := c.Agent(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	input, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunInputBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	sess, status, err := s.startRun(r.Context(), c, agent, input)
	s.writeRun(w, sess, status, err)
}

// handleSessionRerun starts a new session of the same agent with the input
// the session was started with, params as they were filled in then.
func (s *server) handleSessionRerun(w http.ResponseWriter, r *http.Request) {
	old, ok := s.session(w, r)
	if !ok {
		return
	}
	sess, status, err := s.rerun(r.Context(), old)
	s.writeRun(w, sess, status, err)
}

// rerun starts old's agent again with old's input. On error it also returns
// the HTTP status that fits it.
func (s *server) rerun(ctx context.Context, old *sessions.Session) (*sessions.Session, int, error) {
	if old.Input() == nil {
		return nil, http.StatusConflict, errors.New("session has no stored input; it was started before inputs were kept")
	}
	c, status, err := s.readConf(ctx)
	if err != nil {
		return nil, status, err
	}
	agent, err := c.Agent(old.Agent())
	if err != nil {
		return nil, http.StatusNotFound, err
	}
	return s.startRun(ctx, c, agent, old.Input())
}

// writeRun answers a request that started a run: the new session, or the
// error with any missing secrets or params listed.
func (s *server) writeRun(w http.ResponseWriter, sess *sessions.Session, status int, err error) {
	var missing missingSecretsError
	var missingParams missingParamsError
	if errors.As(err, &missing) {
		writeJSON(w, status, map[string]any{"error": err.Error(), "missing_secrets": missing})
		return
	} else if errors.As(err, &missingParams) {
		writeJSON(w, status, map[string]any{"error": err.Error(), "missing_params": missingParams})
		return
	} else if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"session_id": sess.ID(), "agent": sess.Agent(), "status": sessions.Running})
}

// startRun checks input against the agent and starts its image in the
// background. On error it also returns the HTTP status that fits it.
func (s *server) startRun(ctx context.Context, c *conf.Conf, agent *conf.Agent, input []byte) (*sessions.Session, int, error) {
	// An agent with an input schema takes a JSON object matching it, with
	// properties left out taken from the config's params; others take a
	// free-form task.
	if agent.InputSchema != nil {
		var fields map[string]any
		if err := json.Unmarshal(input, &fields); err != nil {
			return nil, http.StatusUnprocessableEntity, errors.New("input must be a JSON object: " + err.Error())
		}
		params, err := s.paramValues(ctx, c)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		filled, missing := c.FillParams(agent, fields, params)
		if len(missing) > 0 {
			return nil, http.StatusUnprocessableEntity, missingParamsError(missing)
		}
		if filled {
			input, _ = json.Marshal(fields)
		}
		if err := agent.InputSchema.Validate(fields); err != nil {
			return nil, http.StatusUnprocessableEntity, errors.New("input: " + err.Error())
		}
	}

	// Checked up front so a missing image is a 409 here, not a failed session.
	name := agent.Name
	tag := build.ImageTag(name)
	if built, err := s.docker.ImageExists(ctx, tag); err != nil {
		s.logger.Error("check image", "image", tag, "err", err)
		return nil, http.StatusInternalServerError, err
	} else if !built {
		return nil, http.StatusConflict, errors.New("agent not built; POST /agents/" + name + "/build first")
	}

	secrets, err := s.secretEnv(ctx, build.AgentSecrets(c, agent))
	var missing missingSecretsError
	if errors.As(err, &missing) {
		return nil, http.StatusUnprocessableEntity, err
	} else if err != nil {
		s.logger.Error("read secrets", "agent", name, "err", err)
		return nil, http.StatusInternalServerError, errors.New("failed to read secrets")
	}

	// Limits come from the current config, so changing them needs no rebuild.
	limits := c.AgentLimits(agent)
	limitsJSON, _ := json.Marshal(limits)
	// The run outlives the request; its timeout comes from the limits, and
	// its context ends early on a stop.
	sess := s.sessions.Start(name, limits, input, func(ctx context.Context, sess *sessions.Session) (*sessions.Result, error) {
		res, err := s.docker.Run(ctx, tag, input, build.RunOptions{
			Env:       map[string]string{"TREFOIL_LIMITS": string(limitsJSON)},
			Timeout:   time.Duration(limits.TimeoutSeconds * float64(time.Second)),
			Logs:      sess,
			Secrets:   secrets,
			Artifacts: sess.ArtifactsDir(),
		})
		if err != nil {
			s.logger.Error("run agent", "agent", name, "session", sess.ID(), "err", err)
			return nil, err
		}
		s.logger.Info("ran agent", "agent", name, "session", sess.ID(), "exit_code", res.ExitCode)
		return &sessions.Result{ExitCode: res.ExitCode, TimedOut: res.TimedOut, Stdout: res.Stdout,
			Structured: len(agent.Outputs) > 0}, nil
	})
	return sess, http.StatusAccepted, nil
}

// session looks up the session in the path and checks it belongs to the
// agent in the path.
func (s *server) session(w http.ResponseWriter, r *http.Request) (*sessions.Session, bool) {
	sess, ok := s.sessions.Get(r.PathValue("id"))
	if !ok || sess.Agent() != r.PathValue("name") {
		writeError(w, http.StatusNotFound, "session not found")
		return nil, false
	}
	return sess, true
}

func (s *server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.session(w, r); ok {
		writeJSON(w, http.StatusOK, sess.Status())
	}
}

// handleSessionStop stops a running session; its container is killed and
// the session finishes as failed with the error "stopped". A session that
// already finished is a 409.
func (s *server) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if !sess.Stop() {
		writeError(w, http.StatusConflict, "session not running")
		return
	}
	s.logger.Info("stopping session", "agent", sess.Agent(), "session", sess.ID())
	writeJSON(w, http.StatusAccepted, sess.Status())
}

// handleSessionEvents returns the run's timeline events, parsed from the
// "x-trefoil-event:" lines in its logs.
func (s *server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.session(w, r); ok {
		logs, _ := sess.Logs(0)
		writeJSON(w, http.StatusOK, map[string]any{"events": sessions.ParseEvents(logs), "status": sess.Status().Status})
	}
}

// handleSessionLogs returns the logs from ?offset= on, plus the offset to
// pass next time, so clients can follow a running session. With ?download=1
// it sends the whole log, timeline events included, as a plain-text file.
func (s *server) handleSessionLogs(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("download") != "" {
		logs, _ := sess.Logs(0)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.log"`, r.PathValue("name"), sess.Status().ID))
		io.WriteString(w, logs)
		return
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}
	logs, next := sess.Logs(offset)
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "next_offset": next, "status": sess.Status().Status})
}

// handleSessionResult returns the agent's answer once the run has finished.
// For an agent with outputs, "output" is the answer parsed as JSON.
func (s *server) handleSessionResult(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	info := sess.Status()
	if info.Status == sessions.Running {
		writeError(w, http.StatusConflict, "session still running")
		return
	}
	res := sess.Result()
	if res == nil {
		writeError(w, http.StatusConflict, "session has no result: "+info.Error)
		return
	}
	body := map[string]any{"status": info.Status, "exit_code": res.ExitCode, "stdout": res.Stdout}
	if res.TimedOut {
		body["timed_out"] = true
	}
	if artifacts := sess.Artifacts(); len(artifacts) > 0 {
		body["artifacts"] = artifacts
	}
	var output any
	if res.Structured && info.Status == sessions.Succeeded && json.Unmarshal([]byte(res.Stdout), &output) == nil {
		body["output"] = output
	}
	writeJSON(w, http.StatusOK, body)
}

// handleSessionArtifacts lists the files the run saved as artifacts.
func (s *server) handleSessionArtifacts(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.session(w, r); ok {
		writeJSON(w, http.StatusOK, map[string]any{"artifacts": sess.Artifacts(), "status": sess.Status().Status})
	}
}

// handleSessionArtifact serves one artifact. Artifacts are whatever the
// agent wrote, so the response is sandboxed: an HTML artifact can't run
// script against the dashboard's origin.
func (s *server) handleSessionArtifact(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	f, info, err := sess.OpenArtifact(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

func (s *server) loadConf(w http.ResponseWriter, r *http.Request) (*conf.Conf, bool) {
	c, status, err := s.readConf(r.Context())
	if err != nil {
		writeError(w, status, err.Error())
		return nil, false
	}
	return c, true
}

// readConf loads the stored config with backend settings applied. On error
// it also returns the HTTP status that fits it.
func (s *server) readConf(ctx context.Context) (*conf.Conf, int, error) {
	data, err := s.store.ReadPath(ctx, confPath)
	if errors.Is(err, store.ErrNotFound) {
		return nil, http.StatusConflict, errors.New("no config uploaded; POST /conf first")
	}
	if err != nil {
		s.logger.Error("read conf", "err", err)
		return nil, http.StatusInternalServerError, errors.New("failed to read config")
	}
	c, err := conf.Parse(data)
	if err != nil {
		return nil, http.StatusInternalServerError, errors.New("stored config is invalid: " + err.Error())
	}
	// Re-checked here because backend settings may have changed since upload.
	if err := s.applyProvider(c); err != nil {
		return nil, http.StatusUnprocessableEntity, err
	}
	if err := s.attachSharedModules(c); err != nil {
		return nil, http.StatusUnprocessableEntity, err
	}
	return c, http.StatusOK, nil
}

// secretEnvPrefix namespaces secrets in the server's environment, so a config
// can only reach variables meant for agents.
const secretEnvPrefix = "TREFOIL_SECRET_"

// missingSecretsError lists required secrets that have no value yet.
type missingSecretsError []string

func (e missingSecretsError) Error() string {
	return fmt.Sprintf("missing required secrets: %s; provide each with POST /secret/<name>", strings.Join(e, ", "))
}

// secretEnv returns each secret keyed <NAME>, the variable the agent sees.
// A value stored with POST /secret/{name} wins over TREFOIL_SECRET_<NAME> in
// the server's environment. Required secrets with neither are a
// missingSecretsError.
func (s *server) secretEnv(ctx context.Context, secrets map[string]conf.Secret) (map[string]string, error) {
	env := map[string]string{}
	var missing missingSecretsError
	for name, sec := range secrets {
		key := strings.ToUpper(name)
		v, err := s.readSecret(ctx, key)
		if err != nil {
			return nil, err
		}
		if v == "" {
			v = os.Getenv(secretEnvPrefix + key)
		}
		if v != "" {
			env[key] = v
		} else if sec.Required {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, missing
	}
	return env, nil
}
