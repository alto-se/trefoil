package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"trefoil/api/internal/build"
	"trefoil/api/internal/conf"
	"trefoil/api/internal/jobs"
	"trefoil/api/internal/modules"
	"trefoil/api/internal/schema"
	"trefoil/api/internal/sessions"
	"trefoil/api/internal/store"
)

const (
	confPath     = "conf.json"
	maxConfBytes = 1 << 20
)

type server struct {
	logger    *slog.Logger
	store     store.Store
	validator *schema.Validator
	docker    build.Docker
	jobs      *jobs.Manager
	sessions  *sessions.Manager
	buildDir  string
	modules   *modules.Library
	providers []string
}

type Option func(*server)

// WithProviders limits configs to these LLM providers. The first is used for
// configs that don't set defaults.provider.
func WithProviders(providers []string) Option {
	return func(s *server) { s.providers = providers }
}

// WithManagers replaces the default in-memory job and session managers,
// e.g. with ones from jobs.Open and sessions.Open that persist to disk.
func WithManagers(j *jobs.Manager, sess *sessions.Manager) Option {
	return func(s *server) { s.jobs, s.sessions = j, sess }
}

// New returns the root HTTP handler with all routes registered. Build
// contexts are written to buildDir/<agent>/<job id>; shared modules ("core:os") are
// read from lib.
func New(logger *slog.Logger, st store.Store, v *schema.Validator, d build.Docker, buildDir string, lib *modules.Library, opts ...Option) http.Handler {
	s := &server{logger: logger, store: st, validator: v, docker: d, jobs: jobs.NewManager(), sessions: sessions.NewManager(), buildDir: buildDir, modules: lib}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("POST /conf", s.handlePostConf)
	mux.HandleFunc("GET /conf", s.handleGetConf)
	mux.HandleFunc("GET /agents", s.handleListAgents)
	mux.HandleFunc("GET /secrets", s.handleListSecrets)
	mux.HandleFunc("POST /secret/{id}", s.handlePostSecret)
	mux.HandleFunc("GET /params", s.handleListParams)
	mux.HandleFunc("POST /param/{name}", s.handlePostParam)
	mux.HandleFunc("DELETE /param/{name}", s.handleDeleteParam)
	mux.HandleFunc("GET /jobs", s.handleListJobs)
	mux.HandleFunc("GET /jobs/{id}", s.handleGetJob)
	mux.HandleFunc("POST /jobs/{id}/rerun", s.handleJobRerun)
	// One pattern serves /agents/{id}/build, /agents/{id}/run and the
	// /agents/build/{id} alias; separate patterns would conflict in ServeMux.
	mux.HandleFunc("POST /agents/{a}/{b}", s.handleAgentAction)
	mux.HandleFunc("GET /agents/{name}/context", s.handleAgentContext)
	mux.HandleFunc("GET /export", s.handleExport)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/status", s.handleSessionStatus)
	mux.HandleFunc("POST /agents/{name}/sessions/{id}/stop", s.handleSessionStop)
	mux.HandleFunc("POST /agents/{name}/sessions/{id}/rerun", s.handleSessionRerun)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/logs", s.handleSessionLogs)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/events", s.handleSessionEvents)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/result", s.handleSessionResult)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/artifacts", s.handleSessionArtifacts)
	mux.HandleFunc("GET /agents/{name}/sessions/{id}/artifacts/{path...}", s.handleSessionArtifact)
	s.registerUI(mux)
	return logRequests(logger, mux)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) handlePostConf(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	body = conf.Upgrade(body)
	if err := s.validator.Validate(body); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c, err := conf.Parse(body)
	if err == nil {
		err = c.Validate()
	}
	if err == nil {
		err = s.applyProvider(c)
	}
	if err == nil {
		err = s.attachSharedModules(c)
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Generate every agent's build context up front so a config that can't
	// be built is rejected here rather than at build time.
	contexts := map[string]*build.Context{}
	for i := range c.Agents {
		bc, err := build.Compose(c, &c.Agents[i])
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		contexts[c.Agents[i].Name] = bc
	}

	if err := s.store.WritePath(r.Context(), confPath, body); err != nil {
		s.logger.Error("write conf", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to store config")
		return
	}
	paths := []string{}
	for name, bc := range contexts {
		written, err := s.writeContext(r, path.Join("agents", name), bc)
		if err != nil {
			s.logger.Error("write build context", "agent", name, "err", err)
			writeError(w, http.StatusInternalServerError, "failed to store build context")
			return
		}
		paths = append(paths, written...)
	}
	sort.Strings(paths)
	writeJSON(w, http.StatusCreated, map[string]any{"status": "stored", "files": paths})
}

// writeContext stores a build context (Dockerfile plus files) under prefix
// and returns the paths it wrote.
func (s *server) writeContext(r *http.Request, prefix string, bc *build.Context) ([]string, error) {
	files := map[string][]byte{"Dockerfile": []byte(bc.Dockerfile)}
	for name, data := range bc.Files {
		files[name] = data
	}
	written := make([]string, 0, len(files))
	for name, data := range files {
		p := path.Join(prefix, name)
		if err := s.store.WritePath(r.Context(), p, data); err != nil {
			return nil, err
		}
		written = append(written, p)
	}
	return written, nil
}

func (s *server) handleGetConf(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.ReadPath(r.Context(), confPath)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no config stored")
		return
	}
	if err != nil {
		s.logger.Error("read conf", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read config")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start))
	})
}

// statusRecorder remembers the status code a handler wrote. Handlers that
// never call WriteHeader get net/http's implicit 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// applyProvider fills in the default provider and rejects providers the
// backend doesn't allow. With no allowlist, any provider goes.
func (s *server) applyProvider(c *conf.Conf) error {
	if len(s.providers) == 0 {
		return nil
	}
	if c.Defaults.Provider == "" {
		c.Defaults.Provider = s.providers[0]
		return nil
	}
	if !slices.Contains(s.providers, c.Defaults.Provider) {
		return fmt.Errorf("provider %q not enabled on this server (llm_provider: %s)",
			c.Defaults.Provider, strings.Join(s.providers, ", "))
	}
	return nil
}

// attachSharedModules loads every shared module ("<namespace>:<name>") the
// config uses into c.Modules. The stored config keeps only the reference, so
// edits to the shared folder apply on the next build.
func (s *server) attachSharedModules(c *conf.Conf) error {
	for _, name := range c.ModuleRefs() {
		if !modules.IsShared(name) {
			continue
		}
		if _, local := c.Modules[name]; local {
			return fmt.Errorf("module %q: shared module names can't be defined locally", name)
		}
		raw, b, err := s.modules.Load(name)
		if err != nil {
			return err
		}
		if err := s.validator.ValidateModule(raw); err != nil {
			return fmt.Errorf("shared module %q: %w", name, err)
		}
		if c.Modules == nil {
			c.Modules = map[string]conf.Module{}
		}
		c.Modules[name] = *b
	}
	return nil
}
