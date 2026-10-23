package sessions

import (
	"encoding/json"
	"strings"
	"time"
)

// EventPrefix starts a timeline event line in a run's logs. The harness
// writes events as "x-trefoil-event: <type> <json object>", one per line, on
// stderr next to its human-readable output.
const EventPrefix = "x-trefoil-event: "

// Event is one entry on a run's timeline. Data is the event's JSON payload;
// its "ts" field (Unix seconds) becomes At.
type Event struct {
	Type string         `json:"type"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data"`
}

// ParseEvents picks the event lines out of logs. Lines that don't parse are
// skipped, as is a last line that hasn't been finished yet.
func ParseEvents(logs string) []Event {
	events := []Event{}
	for len(logs) > 0 {
		line, rest, complete := strings.Cut(logs, "\n")
		logs = rest
		if !complete {
			break
		}
		if e, ok := parseEvent(line); ok {
			events = append(events, e)
		}
	}
	return events
}

func parseEvent(line string) (Event, bool) {
	body, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), EventPrefix)
	if !ok {
		return Event{}, false
	}
	kind, payload, _ := strings.Cut(body, " ")
	if kind == "" {
		return Event{}, false
	}
	e := Event{Type: kind, Data: map[string]any{}}
	if strings.TrimSpace(payload) != "" {
		if err := json.Unmarshal([]byte(payload), &e.Data); err != nil {
			return Event{}, false
		}
	}
	if ts, ok := e.Data["ts"].(float64); ok {
		e.At = time.UnixMilli(int64(ts * 1000))
		delete(e.Data, "ts")
	}
	return e, true
}

// StripEvents returns logs without their event lines, for showing to people.
func StripEvents(logs string) string {
	if !strings.Contains(logs, EventPrefix) {
		return logs
	}
	var b strings.Builder
	for len(logs) > 0 {
		line, rest, complete := strings.Cut(logs, "\n")
		logs = rest
		if strings.HasPrefix(line, EventPrefix) {
			continue
		}
		b.WriteString(line)
		if complete {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
