package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/Bob-xisuke/orb-config-dispatch/internal/store"
)

type rolloutEntryPayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type registerRolloutPayload struct {
	ConfigName        string                `json:"configName"`
	TargetEnvironment string                `json:"targetEnvironment"`
	Namespace         string                `json:"namespace"`
	Version           *int64                `json:"version"`
	Entries           []rolloutEntryPayload `json:"entries"`
}

type rolloutEntryView struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type rolloutView struct {
	ConfigName        string             `json:"configName"`
	TargetEnvironment string             `json:"targetEnvironment"`
	Namespace         string             `json:"namespace"`
	Version           int64              `json:"version"`
	Entries           []rolloutEntryView `json:"entries"`
	Digest            string             `json:"digest"`
	Revoked           bool               `json:"revoked"`
}

const (
	codeInvalidInput = "InvalidConfigInputError"
	codeNotFound     = "ConfigRolloutNotFoundError"
	codeConflict     = "ConfigConflictError"
	codeStorage      = "storage_unavailable"
)

func writeAPIError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func nonBlank(value string) bool { return strings.TrimSpace(value) != "" }

func registerRolloutHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var payload registerRolloutPayload
		decoder := json.NewDecoder(c.Request.Body)
		if err := decoder.Decode(&payload); err != nil {
			writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "request body must be a valid config rollout object")
			return
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "request body must contain a single JSON object")
			return
		}

		if !nonBlank(payload.ConfigName) || !nonBlank(payload.TargetEnvironment) ||
			!nonBlank(payload.Namespace) || payload.Version == nil {
			writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "configName, targetEnvironment, namespace and version are required")
			return
		}
		if *payload.Version <= 0 {
			writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "version must be a decimal integer greater than zero")
			return
		}
		if len(payload.Entries) == 0 {
			writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "entries must contain at least one item")
			return
		}

		entries := make([]store.Entry, len(payload.Entries))
		seenKeys := make(map[string]struct{}, len(payload.Entries))
		for index, item := range payload.Entries {
			if !nonBlank(item.Key) || !nonBlank(item.Value) {
				writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "every entry requires non-empty key and value strings")
				return
			}
			if _, exists := seenKeys[item.Key]; exists {
				writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "entry keys must not repeat")
				return
			}
			seenKeys[item.Key] = struct{}{}
			entries[index] = store.Entry{Key: item.Key, Value: item.Value}
		}

		rollout := &store.Rollout{
			ConfigName:        payload.ConfigName,
			TargetEnvironment: payload.TargetEnvironment,
			Namespace:         payload.Namespace,
			Version:           *payload.Version,
			Entries:           entries,
		}
		stored, err := st.Register(c.Request.Context(), rollout)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeAPIError(c, http.StatusConflict, codeConflict, "a config rollout with the same identity but different content already exists")
				return
			}
			writeAPIError(c, http.StatusServiceUnavailable, codeStorage, "database is not available")
			return
		}
		c.JSON(http.StatusCreated, rolloutToView(stored))
	}
}

func listRolloutsHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		query := c.Request.URL.Query()
		allowed := map[string]struct{}{
			"targetEnvironment": {},
			"namespace":         {},
			"configName":        {},
		}
		for name := range query {
			if _, ok := allowed[name]; !ok {
				writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "only targetEnvironment, namespace and configName query parameters are accepted")
				return
			}
		}
		targetEnvironment := query.Get("targetEnvironment")
		namespace := query.Get("namespace")
		configName := query.Get("configName")
		if !nonBlank(targetEnvironment) || !nonBlank(namespace) || !nonBlank(configName) {
			writeAPIError(c, http.StatusBadRequest, codeInvalidInput, "targetEnvironment, namespace and configName must all be non-empty")
			return
		}

		rollouts, err := st.List(c.Request.Context(), targetEnvironment, namespace, configName)
		if err != nil {
			writeAPIError(c, http.StatusServiceUnavailable, codeStorage, "database is not available")
			return
		}
		if len(rollouts) == 0 {
			writeAPIError(c, http.StatusNotFound, codeNotFound, "no config rollout matches the query")
			return
		}

		items := make([]rolloutView, len(rollouts))
		for index := range rollouts {
			items[index] = rolloutToView(&rollouts[index])
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

func rolloutToView(rollout *store.Rollout) rolloutView {
	entries := make([]rolloutEntryView, len(rollout.Entries))
	for index, item := range rollout.Entries {
		entries[index] = rolloutEntryView{Key: item.Key, Value: item.Value}
	}
	return rolloutView{
		ConfigName:        rollout.ConfigName,
		TargetEnvironment: rollout.TargetEnvironment,
		Namespace:         rollout.Namespace,
		Version:           rollout.Version,
		Entries:           entries,
		Digest:            rollout.Digest,
		Revoked:           rollout.Revoked,
	}
}
