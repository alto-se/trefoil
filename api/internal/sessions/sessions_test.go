package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"trefoil/api/internal/conf"
)

func wait(t *testing.T, s *Session) StatusInfo {
	t.Helper()
	for i := 0; i < 200; i++ {
		if info := s.Status(); info.Status != Running {
			return info
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("session did not finish")
	return StatusInfo{}
}

func TestStatus(t *testing.T) {
	m := NewManager()
	cases := []struct {
		name   string
		res    *Result
		err    error
		status Status
		errMsg string
	}{
		{"ok", &Result{}, nil, Succeeded, ""},
		{"exit 1", &Result{ExitCode: 1}, nil, Failed, ""},
		{"limit", &Result{ExitCode: ExitLimit}, nil, Failed, "stopped at a limit"},
		{"timeout", &Result{ExitCode: 124, TimedOut: true}, nil, Failed, "timed out (limits.timeout_seconds)"},
		{"error", nil, errors.New("docker run: boom"), Failed, "docker run: boom"},
	}
	for _, tc := range cases {
		s := m.Start("a", conf.Limits{}, nil, func(context.Context, *Session) (*Result, error) { return tc.res, tc.err })
		info := wait(t, s)
		if info.Status != tc.status || info.Error != tc.errMsg || info.FinishedAt == nil {
			t.Errorf("%s: %+v", tc.name, info)
		}
		if got, ok := m.Get(s.ID()); !ok || got != s {
			t.Errorf("%s: Get(%s) failed", tc.name, s.ID())
		}
	}
}

func TestStop(t *testing.T) {
	s := NewManager().Start("a", conf.Limits{}, nil, func(ctx context.Context, _ *Session) (*Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !s.Stop() {
		t.Fatal("Stop on a running session = false")
	}
	if s.Stop() {
		t.Error("second Stop = true")
	}
	info := wait(t, s)
	if info.Status != Failed || info.Error != "stopped" {
		t.Fatalf("after Stop: %+v", info)
	}
	if logs, _ := s.Logs(0); !strings.Contains(logs, "stopped by request") {
		t.Errorf("logs = %q", logs)
	}
	if s.Stop() {
		t.Error("Stop on a finished session = true")
	}
}

func TestLogs(t *testing.T) {
	s := NewManager().Start("a", conf.Limits{}, nil, func(_ context.Context, s *Session) (*Result, error) {
		s.Write([]byte("hello "))
		s.Write([]byte("world"))
		return &Result{}, nil
	})
	wait(t, s)
	if logs, next := s.Logs(0); logs != "hello world" || next != 11 {
		t.Fatalf("Logs(0) = %q, %d", logs, next)
	}
	if logs, _ := s.Logs(6); logs != "world" {
		t.Fatalf("Logs(6) = %q", logs)
	}
	if logs, next := s.Logs(99); logs != "" || next != 11 {
		t.Fatalf("Logs(99) = %q, %d", logs, next)
	}
}

func TestOpenPersists(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := m.Start("echo", conf.Limits{}, []byte(`{"task":"hi"}`), func(_ context.Context, s *Session) (*Result, error) {
		s.Write([]byte("hello\n"))
		return &Result{ExitCode: 0, Stdout: `{"a":1}`, Structured: true}, nil
	})
	wait(t, s)

	m2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m2.Get(s.ID())
	if !ok {
		t.Fatal("session not reloaded")
	}
	if info := got.Status(); info.Status != Succeeded || info.Agent != "echo" {
		t.Errorf("status = %+v", info)
	}
	if logs, _ := got.Logs(0); logs != "hello\n" {
		t.Errorf("logs = %q", logs)
	}
	if res := got.Result(); res == nil || res.Stdout != `{"a":1}` || !res.Structured {
		t.Errorf("result = %+v", res)
	}
	if in := string(got.Input()); in != `{"task":"hi"}` {
		t.Errorf("input = %q", in)
	}
}

func TestOpenMarksRunningFailed(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	s := m.Start("echo", conf.Limits{}, nil, func(_ context.Context, s *Session) (*Result, error) {
		<-release
		return &Result{}, nil
	})

	m2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m2.Get(s.ID())
	if info := got.Status(); info.Status != Failed || info.Error != "interrupted by server restart" {
		t.Errorf("status = %+v", info)
	}
	close(release)
	wait(t, s) // let the run finish before TempDir is removed
}

func TestArtifacts(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := m.Start("pdf", conf.Limits{}, nil, func(_ context.Context, s *Session) (*Result, error) {
		art := s.ArtifactsDir()
		_ = os.MkdirAll(filepath.Join(art, "sub"), 0o755)
		_ = os.WriteFile(filepath.Join(art, "page.pdf"), []byte("%PDF"), 0o644)
		_ = os.WriteFile(filepath.Join(art, "sub", "b.txt"), []byte("b"), 0o644)
		// A container could leave a symlink pointing at the host.
		_ = os.WriteFile(filepath.Join(dir, "secret"), []byte("x"), 0o644)
		_ = os.Symlink(filepath.Join(dir, "secret"), filepath.Join(art, "escape"))
		return &Result{}, nil
	})
	wait(t, s)

	want := []Artifact{{Path: "page.pdf", Size: 4}, {Path: "sub/b.txt", Size: 1}}
	if got := s.Artifacts(); !reflect.DeepEqual(got, want) {
		t.Errorf("artifacts = %+v, want %+v", got, want)
	}
	f, info, err := s.OpenArtifact("sub/b.txt")
	if err != nil || info.Size() != 1 {
		t.Fatalf("open sub/b.txt: %v %v", info, err)
	}
	f.Close()
	for _, name := range []string{"escape", "../" + s.ID() + ".json", "/etc/passwd", "sub", "missing"} {
		if f, _, err := s.OpenArtifact(name); err == nil {
			f.Close()
			t.Errorf("OpenArtifact(%q) succeeded", name)
		}
	}

	if got := NewManager().Start("x", conf.Limits{}, nil, func(context.Context, *Session) (*Result, error) { return &Result{}, nil }).Artifacts(); len(got) != 0 {
		t.Errorf("in-memory artifacts = %v", got)
	}
}
