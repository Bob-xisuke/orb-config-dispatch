package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-config-dispatch/internal/store"
)

const (
	codeInvalidConfigInput    = "InvalidConfigInputError"
	codeConfigRolloutNotFound = "ConfigRolloutNotFoundError"
	codeConfigConflict        = "ConfigConflictError"
	codeStorageUnavailable    = "storage_unavailable"
	messageStorageUnavailable = "database is not available"
)

var decimalInteger = regexp.MustCompile(`^[0-9]+$`)

// rolloutRequest is the accepted shape of POST /v1/config-rollouts.
type rolloutRequest struct {
	ConfigName        string          `json:"configName"`
	TargetEnvironment string          `json:"targetEnvironment"`
	Namespace         string          `json:"namespace"`
	Version           json.RawMessage `json:"version"`
	Entries           []store.Entry   `json:"entries"`
}

// registerRollout handles POST /v1/config-rollouts.
func registerRollout(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body rolloutRequest
		decoder := json.NewDecoder(c.Request.Body)
		if err := decoder.Decode(&body); err != nil {
			writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "request body must be a single JSON object")
			return
		}
		if decoder.More() {
			writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "request body must be a single JSON object")
			return
		}
		if body.ConfigName == "" || body.TargetEnvironment == "" || body.Namespace == "" {
			writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "configName, targetEnvironment and namespace must be non-empty strings")
			return
		}
		version, ok := parseVersion(body.Version)
		if !ok {
			writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "version must be a decimal integer greater than zero")
			return
		}
		if len(body.Entries) == 0 {
			writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "entries must contain at least one item")
			return
		}
		seen := make(map[string]struct{}, len(body.Entries))
		for _, entry := range body.Entries {
			if entry.Key == "" || entry.Value == "" {
				writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "entry key and value must be non-empty strings")
				return
			}
			if _, exists := seen[entry.Key]; exists {
				writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "entry keys must not repeat")
				return
			}
			seen[entry.Key] = struct{}{}
		}

		rollout := store.Rollout{
			ConfigName:        body.ConfigName,
			TargetEnvironment: body.TargetEnvironment,
			Namespace:         body.Namespace,
			Version:           version,
			Entries:           body.Entries,
		}
		stored, _, err := st.CreateRollout(c.Request.Context(), rollout)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeError(c, http.StatusConflict, codeConfigConflict, "a different rollout already exists for this configName, targetEnvironment, namespace and version")
				return
			}
			writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, messageStorageUnavailable)
			return
		}
		c.JSON(http.StatusCreated, stored)
	}
}

// listRollouts handles GET /v1/config-rollouts.
func listRollouts(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		environment := c.Query("targetEnvironment")
		namespace := c.Query("namespace")
		configName := c.Query("configName")
		if environment == "" || namespace == "" || configName == "" {
			writeError(c, http.StatusBadRequest, codeInvalidConfigInput, "targetEnvironment, namespace and configName query parameters must be non-empty")
			return
		}
		rollouts, err := st.ListRollouts(c.Request.Context(), environment, namespace, configName)
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, codeStorageUnavailable, messageStorageUnavailable)
			return
		}
		if len(rollouts) == 0 {
			writeError(c, http.StatusNotFound, codeConfigRolloutNotFound, "no config rollout matches the query")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": rollouts})
	}
}

// parseVersion accepts a JSON number or string holding a decimal integer greater than zero.
func parseVersion(raw json.RawMessage) (int64, bool) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return 0, false
	}
	if strings.HasPrefix(text, `"`) {
		var quoted string
		if err := json.Unmarshal(raw, &quoted); err != nil {
			return 0, false
		}
		text = quoted
	}
	if !decimalInteger.MatchString(text) {
		return 0, false
	}
	version, err := strconv.ParseInt(text, 10, 64)
	if err != nil || version <= 0 {
		return 0, false
	}
	return version, true
}
