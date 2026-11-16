// Package sessions tracks agent runs. A Manager from NewManager keeps
// sessions in memory; one from Open also persists them to a directory so they
// survive a restart.
package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"trefoil/api/internal/conf"
)

type Status string

const (
	Running   Status = "running"
	Succeeded Status = "succeeded"
	// Failed covers a non-zero exit, a timeout, a limit stop, a Stop and
	// errors starting the container; Result and the error say which.
	Failed Status = "failed"
)

// Result is what a finished run produced.
type Result struct {
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Stdout   string `json:"stdout"`
	// Structured is set when the agent declares outputs, so Stdout is JSON.
	Structured bool `json:"-"`
}

// Session is one run of an agent. Its fields are guarded by mu; read them
// with Status, Logs and Result.
type Session struct {
	mu         sync.Mutex
	id         string
	agent      string
	limits     conf.Limits
	input      []byte
	status     Status
	logs       strings.Builder
	result     *Result
	err        string
	createdAt  time.Time
	finishedAt time.Time
	// dir is where the session is persisted; empty for in-memory sessions.
	// logFile is open while the run is going.
	dir     string
	logFile *os.File
	// cancel ends the run's context; stopped records that Stop called it.
	cancel  context.CancelFunc
	stopped bool
}

// StatusInfo is a point-in-time view of a session, without logs or output.
type StatusInfo struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Status Status `json:"status"`
	// Limits are the budgets the run was started with.
	Limits     conf.Limits `json:"limits,omitzero"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	TimedOut   bool        `json:"timed_out,omitempty"`
	Error      string      `json:"error,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
}

func (s *Session) ID() string    { return s.id }
func (s *Session) Agent() string { return s.agent }

// Input is what the run was started with on stdin, params filled in, so a
// rerun gets the same values. It is nil for sessions saved before inputs
// were kept.
func (s *Session) Input() []byte { return s.input }

// Write appends to the session's logs, so it can be handed to the container
// as its stderr.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logFile != nil {
		_, _ = s.logFile.Write(p)
	}
	return s.logs.Write(p)
}

func (s *Session) Status() StatusInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := StatusInfo{ID: s.id, Agent: s.agent, Status: s.status, Limits: s.limits, Error: s.err, CreatedAt: s.createdAt}
	if s.result != nil {
		code := s.result.ExitCode
		info.ExitCode, info.TimedOut = &code, s.result.TimedOut
	}
	if !s.finishedAt.IsZero() {
		t := s.finishedAt
		info.FinishedAt = &t
	}
	return info
}

// Stop cancels a running session's context, which kills its container. It
// reports false when the session isn't running. The session finishes as
// failed with the error "stopped" once the run has wound down.
func (s *Session) Stop() bool {
	s.mu.Lock()
	if s.status != Running || s.cancel == nil || s.stopped {
		s.mu.Unlock()
		return false
	}
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
	_, _ = s.Write([]byte("\n[trefoil] run stopped by request\n"))
	return true
}

// Logs returns the logs from byte offset on, and the offset to ask for next.
func (s *Session) Logs(offset int) (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	logs := s.logs.String()
	offset = min(max(offset, 0), len(logs))
	return logs[offset:], len(logs)
}

// Artifact is a file the run left in its artifacts directory.
type Artifact struct {
	// Path is slash-separated and relative to the artifacts directory.
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ArtifactsDir is the host directory the run's artifacts are collected into,
// <dir>/<id>/artifacts; empty for in-memory sessions, which keep none.
func (s *Session) ArtifactsDir() string {
	if s.dir == "" {
		return ""
	}
	return filepath.Join(s.dir, s.id, "artifacts")
}

// Artifacts lists the regular files in the artifacts directory, sorted by
// path. Symlinks and other special files are left out.
func (s *Session) Artifacts() []Artifact {
	out := []Artifact{}
	dir := s.ArtifactsDir()
	if dir == "" {
		return out
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, Artifact{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	return out
}

// OpenArtifact opens one artifact by its Path. The lookup can't leave the
// artifacts directory, even through a symlink the container left there.
func (s *Session) OpenArtifact(name string) (*os.File, fs.FileInfo, error) {
	dir := s.ArtifactsDir()
	if dir == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
		return nil, nil, fs.ErrNotExist
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, fs.ErrNotExist
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fs.ErrNotExist
	}
	return f, info, nil
}

// Result returns the run's result, or nil while it is running or if it
// failed before the container produced one.
func (s *Session) Result() *Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// record is the persisted form of a session; logs live next to it in
// <id>.log.
type record struct {
	ID         string        `json:"id"`
	Agent      string        `json:"agent"`
	Limits     conf.Limits   `json:"limits,omitzero"`
	Input      []byte        `json:"input,omitempty"`
	Status     Status        `json:"status"`
	Result     *storedResult `json:"result,omitempty"`
	Error      string        `json:"error,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	FinishedAt time.Time     `json:"finished_at"`
}

// storedResult persists Result.Structured, which the API leaves out.
type storedResult struct {
	Result
	Structured bool `json:"structured,omitempty"`
}

// save writes the session's record to its dir. Callers hold s.mu.
// Persisting is best effort: the in-memory session stays authoritative.
func (s *Session) save() {
	if s.dir == "" {
		return
	}
	rec := record{ID: s.id, Agent: s.agent, Limits: s.limits, Input: s.input, Status: s.status, Error: s.err,
		CreatedAt: s.createdAt, FinishedAt: s.finishedAt}
	if s.result != nil {
		rec.Result = &storedResult{*s.result, s.result.Structured}
	}
	data, _ := json.Marshal(rec)
	_ = writeFile(filepath.Join(s.dir, s.id+".json"), data)
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	dir      string
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// Open returns a Manager that persists sessions to dir and loads the ones
// already there. Runs that were going when the server stopped are marked
// failed.
func Open(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{sessions: map[string]*Session{}, dir: dir}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r record
		if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
			continue // skip files we can't make sense of rather than refuse to start
		}
		s := &Session{id: r.ID, agent: r.Agent, limits: r.Limits, input: r.Input, status: r.Status, err: r.Error,
			createdAt: r.CreatedAt, finishedAt: r.FinishedAt, dir: dir}
		if r.Result != nil {
			res := r.Result.Result
			res.Structured = r.Result.Structured
			s.result = &res
		}
		logs, err := os.ReadFile(filepath.Join(dir, r.ID+".log"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		s.logs.Write(logs)
		if s.status == Running {
			s.status, s.err = Failed, "interrupted by server restart"
			if s.finishedAt.IsZero() {
				s.finishedAt = time.Now()
			}
			s.save()
		}
		m.sessions[s.id] = s
	}
	return m, nil
}

// Start creates a session and runs fn in the background. fn writes logs to
// the session and should end the run when ctx is done (see Stop); a returned
// error means the run couldn't happen at all. limits are kept with the
// session so its budgets can be shown, and input so it can be rerun.
func (m *Manager) Start(agent string, limits conf.Limits, input []byte, fn func(ctx context.Context, s *Session) (*Result, error)) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{id: newID(), agent: agent, limits: limits, input: input, status: Running, createdAt: time.Now(), dir: m.dir, cancel: cancel}
	if m.dir != "" {
		s.logFile, _ = os.OpenFile(filepath.Join(m.dir, s.id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	s.save()
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	go func() {
		res, err := fn(ctx, s)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.finishedAt, s.result = time.Now(), res
		switch {
		case s.stopped:
			s.status, s.err = Failed, "stopped"
		case err != nil:
			s.status, s.err = Failed, err.Error()
		case res.TimedOut:
			s.status, s.err = Failed, "timed out (limits.timeout_seconds)"
		case res.ExitCode == ExitLimit:
			s.status, s.err = Failed, "stopped at a limit"
		case res.ExitCode != 0:
			s.status = Failed
		default:
			s.status = Succeeded
		}
		s.save()
		if s.logFile != nil {
			s.logFile.Close()
			s.logFile = nil
		}
	}()
	return s
}

// ExitLimit is the harness's exit code when a run limit is reached.
const ExitLimit = 3

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List returns all sessions, newest first.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].createdAt.After(out[b].createdAt) })
	return out
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeFile writes data to path via a temp file and rename, so a crash never
// leaves a half-written record.
func writeFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
