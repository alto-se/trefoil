package jobs

import (
	"errors"
	"testing"
	"time"
)

func wait(t *testing.T, j *Job) Snapshot {
	t.Helper()
	for i := 0; i < 200; i++ {
		if s := j.Snapshot(); s.Status != Queued && s.Status != Running {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Snapshot{}
}

func TestOpenPersists(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := m.Start("build", "echo", "trefoil/echo", "FROM scratch", func(j *Job) error {
		j.SetContextDir("/tmp/ctx")
		j.Write([]byte("step 1\n"))
		return errors.New("boom")
	})
	wait(t, j)

	m2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m2.Get(j.ID())
	if !ok {
		t.Fatal("job not reloaded")
	}
	s := got.Snapshot()
	if s.Status != Failed || s.Error != "boom" || s.Logs != "step 1\n" || s.ContextDir != "/tmp/ctx" ||
		s.Image != "trefoil/echo" || s.StartedAt == nil || s.FinishedAt == nil {
		t.Errorf("snapshot = %+v", s)
	}
	if len(m2.List()) != 1 {
		t.Errorf("list = %d jobs", len(m2.List()))
	}
}

func TestOpenMarksUnfinishedFailed(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	j := m.Start("build", "echo", "", "", func(j *Job) error {
		<-release
		return nil
	})

	m2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := m2.Get(j.ID())
	if s := got.Snapshot(); s.Status != Failed || s.Error != "interrupted by server restart" {
		t.Errorf("snapshot = %+v", s)
	}
	close(release)
	wait(t, j) // let the run finish before TempDir is removed
}
