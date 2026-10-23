// Package jobs tracks long-running work, such as image builds. A Manager
// from NewManager keeps jobs in memory; one from Open also persists them to a
// directory so they survive a restart.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
)

// Job is one unit of background work. Its fields are guarded by mu; use
// Snapshot to read them.
type Job struct {
	mu         sync.Mutex
	id         string
	kind       string
	agent      string
	image      string
	dockerfile string
	contextDir string
	status     Status
	logs       strings.Builder
	err        string
	createdAt  time.Time
	startedAt  time.Time
	finishedAt time.Time
	// dir is where the job is persisted; empty for in-memory jobs. logFile is
	// open while the job runs.
	dir     string
	logFile *os.File
}

// Snapshot is a point-in-time copy of a job, safe to serialize.
type Snapshot struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Agent      string     `json:"agent"`
	Image      string     `json:"image,omitempty"`
	Status     Status     `json:"status"`
	Dockerfile string     `json:"dockerfile,omitempty"`
	ContextDir string     `json:"context_dir,omitempty"`
	Logs       string     `json:"logs"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Write appends to the job's logs so a job can be handed to a process as
// its stdout/stderr.
func (j *Job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.logFile != nil {
		_, _ = j.logFile.Write(p)
	}
	return j.logs.Write(p)
}

func (j *Job) ID() string { return j.id }

// SetContextDir records where the job's build context was written.
func (j *Job) SetContextDir(dir string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.contextDir = dir
	j.save()
}

func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := Snapshot{
		ID:         j.id,
		Kind:       j.kind,
		Agent:      j.agent,
		Image:      j.image,
		Status:     j.status,
		Dockerfile: j.dockerfile,
		ContextDir: j.contextDir,
		Logs:       j.logs.String(),
		Error:      j.err,
		CreatedAt:  j.createdAt,
	}
	if !j.startedAt.IsZero() {
		t := j.startedAt
		s.StartedAt = &t
	}
	if !j.finishedAt.IsZero() {
		t := j.finishedAt
		s.FinishedAt = &t
	}
	return s
}

// record is the persisted form of a job; logs live next to it in <id>.log.
type record struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Agent      string    `json:"agent"`
	Image      string    `json:"image,omitempty"`
	Dockerfile string    `json:"dockerfile,omitempty"`
	ContextDir string    `json:"context_dir,omitempty"`
	Status     Status    `json:"status"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// save writes the job's record to its dir. Callers hold j.mu. Persisting is
// best effort: the in-memory job stays authoritative.
func (j *Job) save() {
	if j.dir == "" {
		return
	}
	data, _ := json.Marshal(record{
		ID: j.id, Kind: j.kind, Agent: j.agent, Image: j.image, Dockerfile: j.dockerfile,
		ContextDir: j.contextDir, Status: j.status, Error: j.err,
		CreatedAt: j.createdAt, StartedAt: j.startedAt, FinishedAt: j.finishedAt,
	})
	_ = writeFile(filepath.Join(j.dir, j.id+".json"), data)
}

type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job
	dir  string
}

func NewManager() *Manager {
	return &Manager{jobs: map[string]*Job{}}
}

// Open returns a Manager that persists jobs to dir and loads the jobs already
// there. Jobs that were unfinished when the server stopped are marked failed.
func Open(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{jobs: map[string]*Job{}, dir: dir}
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
		j := &Job{
			id: r.ID, kind: r.Kind, agent: r.Agent, image: r.Image, dockerfile: r.Dockerfile,
			contextDir: r.ContextDir, status: r.Status, err: r.Error,
			createdAt: r.CreatedAt, startedAt: r.StartedAt, finishedAt: r.FinishedAt, dir: dir,
		}
		logs, err := os.ReadFile(filepath.Join(dir, r.ID+".log"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		j.logs.Write(logs)
		if j.status == Queued || j.status == Running {
			j.status, j.err = Failed, "interrupted by server restart"
			if j.finishedAt.IsZero() {
				j.finishedAt = time.Now()
			}
			j.save()
		}
		m.jobs[j.id] = j
	}
	return m, nil
}

// Start creates a job and runs fn in the background. fn receives the job so
// it can write logs; its error decides whether the job failed.
func (m *Manager) Start(kind, agent, image, dockerfile string, fn func(j *Job) error) *Job {
	j := &Job{
		id:         newID(),
		kind:       kind,
		agent:      agent,
		image:      image,
		dockerfile: dockerfile,
		status:     Queued,
		createdAt:  time.Now(),
		dir:        m.dir,
	}
	if m.dir != "" {
		j.logFile, _ = os.OpenFile(filepath.Join(m.dir, j.id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
	j.save()
	m.mu.Lock()
	m.jobs[j.id] = j
	m.mu.Unlock()

	go func() {
		j.mu.Lock()
		j.status, j.startedAt = Running, time.Now()
		j.save()
		j.mu.Unlock()

		err := fn(j)

		j.mu.Lock()
		defer j.mu.Unlock()
		j.finishedAt = time.Now()
		if err != nil {
			j.status, j.err = Failed, err.Error()
		} else {
			j.status = Succeeded
		}
		j.save()
		if j.logFile != nil {
			j.logFile.Close()
			j.logFile = nil
		}
	}()
	return j
}

func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

// List returns all jobs, newest first.
func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
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
