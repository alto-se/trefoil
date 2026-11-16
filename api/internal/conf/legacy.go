package conf

import "encoding/json"

// Upgrade rewrites a config written with the old `bricks` key (top level,
// defaults and agents) to `modules`, so configs and stored conf.json from
// before the rename still load. Anything it can't parse is returned as is.
func Upgrade(data []byte) []byte {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return data
	}
	changed := rename(top)
	if raw, ok := top["defaults"]; ok {
		var d map[string]json.RawMessage
		if json.Unmarshal(raw, &d) == nil && rename(d) {
			top["defaults"], _ = json.Marshal(d)
			changed = true
		}
	}
	if raw, ok := top["agents"]; ok {
		var agents []map[string]json.RawMessage
		if json.Unmarshal(raw, &agents) == nil {
			found := false
			for _, a := range agents {
				found = rename(a) || found
			}
			if found {
				top["agents"], _ = json.Marshal(agents)
				changed = true
			}
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(top)
	if err != nil {
		return data
	}
	return out
}

func rename(m map[string]json.RawMessage) bool {
	v, ok := m["bricks"]
	if !ok {
		return false
	}
	delete(m, "bricks")
	if _, exists := m["modules"]; !exists {
		m["modules"] = v
	}
	return true
}
