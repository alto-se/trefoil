package sessions

import (
	"testing"
	"time"
)

func TestParseEvents(t *testing.T) {
	logs := "[harness] agent=echo\n" +
		`x-trefoil-event: start {"model":"m","ts":1700000000.5}` + "\n" +
		`x-trefoil-event: tool_call {"id":"t1","tool":"Write","input":{"path":"a"},"ts":1700000001}` + "\n" +
		"x-trefoil-event: broken {not json\n" +
		"x-trefoil-event: bare\n" +
		`x-trefoil-event: done {"turns":2` // unfinished line
	events := ParseEvents(logs)
	if len(events) != 3 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if e := events[0]; e.Type != "start" || e.Data["model"] != "m" || !e.At.Equal(time.UnixMilli(1700000000500)) {
		t.Errorf("start = %+v", e)
	}
	if _, ok := events[0].Data["ts"]; ok {
		t.Error("ts left in data")
	}
	if e := events[1]; e.Type != "tool_call" || e.Data["tool"] != "Write" || e.Data["input"].(map[string]any)["path"] != "a" {
		t.Errorf("tool_call = %+v", e)
	}
	if e := events[2]; e.Type != "bare" || !e.At.IsZero() {
		t.Errorf("bare = %+v", e)
	}
}

func TestStripEvents(t *testing.T) {
	logs := "a\nx-trefoil-event: start {}\nb\nx-trefoil-event: done {}\nc"
	if got := StripEvents(logs); got != "a\nb\nc" {
		t.Errorf("StripEvents = %q", got)
	}
	if got := StripEvents("plain\n"); got != "plain\n" {
		t.Errorf("StripEvents(plain) = %q", got)
	}
}
