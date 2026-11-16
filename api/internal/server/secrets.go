package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"

	"trefoil/api/internal/store"
)

const maxSecretBytes = 64 << 10

// secretName matches the secret names configs may declare (see the schema).
var secretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretPath is where a secret lives in the store. Names are stored upper
// case, so github_token and GITHUB_TOKEN are the same secret.
func secretPath(key string) string {
	return path.Join("secrets", strings.ToUpper(key))
}

// handleListSecrets lists the secrets the config's agents need and where
// each value comes from, never the values themselves.
func (s *server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadConf(w, r)
	if !ok {
		return
	}
	rows, err := s.secretRows(r.Context(), c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// handlePostSecret stores the secret named in the path from a
// {"value": "..."} body. The value is never echoed back.
func (s *server) handlePostSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSecretBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"value": "..."}: `+err.Error())
		return
	}
	if status, err := s.storeSecret(r.Context(), name, req.Value); err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"secret": name, "status": "stored"})
}

// storeSecret validates and stores one secret. On error it also returns the
// HTTP status that fits it.
func (s *server) storeSecret(ctx context.Context, name, value string) (int, error) {
	if !secretName.MatchString(name) {
		return http.StatusBadRequest, errors.New("secret name must match " + secretName.String())
	}
	if value == "" {
		return http.StatusBadRequest, errors.New("value must not be empty")
	}
	if err := s.store.WritePath(ctx, secretPath(name), []byte(value)); err != nil {
		s.logger.Error("write secret", "secret", name, "err", err)
		return http.StatusInternalServerError, errors.New("failed to store secret")
	}
	s.logger.Info("stored secret", "secret", name)
	return http.StatusCreated, nil
}

// readSecret returns a stored secret, or "" if none is stored.
func (s *server) readSecret(ctx context.Context, key string) (string, error) {
	data, err := s.store.ReadPath(ctx, secretPath(key))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return string(data), err
}

// secretSource says where a secret's value would come from at run time:
// "stored" (POST /secret), "env" (TREFOIL_SECRET_<NAME>) or "" for nowhere.
func (s *server) secretSource(ctx context.Context, key string) (string, error) {
	key = strings.ToUpper(key)
	v, err := s.readSecret(ctx, key)
	switch {
	case err != nil:
		return "", err
	case v != "":
		return "stored", nil
	case os.Getenv(secretEnvPrefix+key) != "":
		return "env", nil
	}
	return "", nil
}
