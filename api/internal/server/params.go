package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"trefoil/api/internal/conf"
	"trefoil/api/internal/store"
)

// paramsPath holds the param values set on the server, as one JSON object.
// They outlive config syncs; values for params a config no longer declares
// are ignored.
const paramsPath = "params.json"

const maxParamBytes = 64 << 10

// missingParamsError lists required params a run needs that have no value.
type missingParamsError []string

func (e missingParamsError) Error() string {
	return "params not set: " + strings.Join(e, ", ") + " (set them with POST /param/{name} or on the dashboard's Params page)"
}

// paramRow is one declared param: its schema, effective value and where the
// value comes from: "set" (stored on the server), "default" or "" (none).
type paramRow struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required"`
	Value       any      `json:"value,omitempty"`
	Source      string   `json:"source"`
	Agents      []string `json:"agents"`
}

// readParams returns the stored param values, empty if none are stored.
func (s *server) readParams(ctx context.Context) (map[string]any, error) {
	data, err := s.store.ReadPath(ctx, paramsPath)
	if errors.Is(err, store.ErrNotFound) {
		return map[string]any{}, nil
	} else if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("%s: %w", paramsPath, err)
	}
	return values, nil
}

func (s *server) writeParams(ctx context.Context, values map[string]any) error {
	data, _ := json.MarshalIndent(values, "", "  ")
	return s.store.WritePath(ctx, paramsPath, data)
}

// paramValues returns the effective param values for c (see
// conf.Conf.ParamValues).
func (s *server) paramValues(ctx context.Context, c *conf.Conf) (map[string]any, error) {
	set, err := s.readParams(ctx)
	if err != nil {
		s.logger.Error("read params", "err", err)
		return nil, errors.New("failed to read params")
	}
	return c.ParamValues(set), nil
}

// paramRows lists the config's params in config order.
func (s *server) paramRows(ctx context.Context, c *conf.Conf) ([]paramRow, error) {
	set, err := s.readParams(ctx)
	if err != nil {
		s.logger.Error("read params", "err", err)
		return nil, errors.New("failed to read params")
	}
	rows := []paramRow{}
	for _, p := range c.Params.Props() {
		_, _, optional := p.Parts()
		row := paramRow{Name: p.Name, Type: strings.TrimSuffix(p.Type, "?"), Description: p.Description,
			Required: !optional, Agents: []string{}}
		if v, ok := set[p.Name]; ok {
			row.Value, row.Source = v, "set"
		} else if p.Default != nil {
			row.Value, row.Source = p.Default, "default"
		}
		for i := range c.Agents {
			for _, f := range c.Agents[i].InputSchema.Props() {
				if f.Name == p.Name {
					row.Agents = append(row.Agents, c.Agents[i].Name)
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *server) handleListParams(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	rows, err := s.paramRows(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handlePostParam sets the param named in the path from a {"value": ...}
// body. A string value for a non-string param is parsed by the param's
// type, so `trefoil params set --name=limit 10` stores the integer 10.
func (s *server) handlePostParam(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxParamBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	var req struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Value == nil {
		writeError(w, http.StatusBadRequest, `body must be {"value": ...}`)
		return
	}
	var value any
	_ = json.Unmarshal(req.Value, &value)
	if status, err := s.setParams(r.Context(), c, map[string]any{name: value}, nil); err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"param": name, "status": "stored"})
}

// handleDeleteParam unsets a param, so it falls back to its default.
func (s *server) handleDeleteParam(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if status, err := s.setParams(r.Context(), c, nil, []string{name}); err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"param": name, "status": "unset"})
}

// setParams stores values for, and removes, declared params, after checking
// the new values against the params schema. On error it also returns the
// HTTP status that fits it.
func (s *server) setParams(ctx context.Context, c *conf.Conf, values map[string]any, unset []string) (int, error) {
	props := map[string]conf.Prop{}
	for _, p := range c.Params.Props() {
		props[p.Name] = p
	}
	for name := range values {
		if _, ok := props[name]; !ok {
			return http.StatusNotFound, fmt.Errorf("param %q not declared in the config", name)
		}
	}
	for _, name := range unset {
		if _, ok := props[name]; !ok {
			return http.StatusNotFound, fmt.Errorf("param %q not declared in the config", name)
		}
	}
	for name, v := range values {
		parsed, err := parseParam(props[name], v)
		if err != nil {
			return http.StatusUnprocessableEntity, fmt.Errorf("param %q: %w", name, err)
		}
		values[name] = parsed
	}
	if err := c.Params.ValidatePartial(values); err != nil {
		return http.StatusUnprocessableEntity, fmt.Errorf("params: %w", err)
	}

	stored, err := s.readParams(ctx)
	if err != nil {
		s.logger.Error("read params", "err", err)
		return http.StatusInternalServerError, errors.New("failed to read params")
	}
	for name, v := range values {
		stored[name] = v
	}
	for _, name := range unset {
		delete(stored, name)
	}
	if err := s.writeParams(ctx, stored); err != nil {
		s.logger.Error("write params", "err", err)
		return http.StatusInternalServerError, errors.New("failed to store params")
	}
	names := append(sortedKeys(values), unset...)
	s.logger.Info("stored params", "params", names)
	return http.StatusCreated, nil
}

// parseParam turns a string value for a non-string param into the param's
// type: a scalar for numbers and booleans, JSON for anything else.
func parseParam(p conf.Prop, v any) (any, error) {
	str, ok := v.(string)
	base, list, _ := p.Parts()
	if !ok || (base == "string" && !list) || base == "any" {
		return v, nil
	}
	if !list && (base == "integer" || base == "number" || base == "boolean") {
		return scalar(base, strings.TrimSpace(str))
	}
	var parsed any
	if err := json.Unmarshal([]byte(str), &parsed); err != nil {
		return nil, fmt.Errorf("want JSON for a %s: %w", p.Type, err)
	}
	return parsed, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
