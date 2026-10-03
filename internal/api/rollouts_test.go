package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Bob-xisuke/orb-config-dispatch/internal/store"
)

func newTestRouter(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), st
}

func doRequest(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	decoded := decodeBody(t, recorder)
	errorObject, ok := decoded["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %s has no top-level error object", recorder.Body.String())
	}
	code, _ := errorObject["code"].(string)
	message, _ := errorObject["message"].(string)
	if code == "" || message == "" {
		t.Fatalf("error object missing code or message: %s", recorder.Body.String())
	}
	for _, leaked := range []string{"sql", "SQL", ".go", "goroutine", "/"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("message %q leaks internal detail %q", message, leaked)
		}
	}
	return code
}

const validRolloutBody = `{
	"configName": "billing",
	"targetEnvironment": "prod",
	"namespace": "payments",
	"version": 2,
	"entries": [
		{"key": "zeta", "value": "9"},
		{"key": "alpha", "value": "1"}
	]
}`

func TestRegisterRolloutReturnsCreatedRecord(t *testing.T) {
	handler, _ := newTestRouter(t)

	recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	decoded := decodeBody(t, recorder)
	if decoded["configName"] != "billing" || decoded["targetEnvironment"] != "prod" || decoded["namespace"] != "payments" {
		t.Fatalf("identity fields wrong: %s", recorder.Body.String())
	}
	if decoded["version"] != float64(2) {
		t.Fatalf("version = %v, want 2", decoded["version"])
	}
	if decoded["revoked"] != false {
		t.Fatalf("revoked = %v, want false", decoded["revoked"])
	}
	digest, _ := decoded["digest"].(string)
	if len(digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", digest)
	}
	entries, _ := decoded["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want 2 items", decoded["entries"])
	}
	first, _ := entries[0].(map[string]any)
	second, _ := entries[1].(map[string]any)
	if first["key"] != "alpha" || second["key"] != "zeta" {
		t.Fatalf("entries not sorted by key: %s", recorder.Body.String())
	}
}

func TestRegisterRolloutDigestIgnoresEntryOrder(t *testing.T) {
	handler, _ := newTestRouter(t)

	first := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody)
	reordered := `{
		"configName": "billing",
		"targetEnvironment": "prod",
		"namespace": "payments",
		"version": 3,
		"entries": [
			{"key": "alpha", "value": "1"},
			{"key": "zeta", "value": "9"}
		]
	}`
	second := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", reordered)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d, %d", first.Code, second.Code)
	}
	if decodeBody(t, first)["digest"] != decodeBody(t, second)["digest"] {
		t.Fatal("digests differ for the same key/value set in different order")
	}
}

func TestDuplicateRegistrationIsIdempotent(t *testing.T) {
	handler, _ := newTestRouter(t)

	first := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody)
	second := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody)
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d, %d, want both 201", first.Code, second.Code)
	}
	if decodeBody(t, first)["digest"] != decodeBody(t, second)["digest"] {
		t.Fatal("duplicate registration changed the digest")
	}

	list := doRequest(t, handler, http.MethodGet, "/v1/config-rollouts?targetEnvironment=prod&namespace=payments&configName=billing", "")
	items, _ := decodeBody(t, list)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
}

func TestConflictingRegistrationReturns409(t *testing.T) {
	handler, _ := newTestRouter(t)

	if recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody); recorder.Code != http.StatusCreated {
		t.Fatalf("seed status = %d", recorder.Code)
	}
	conflicting := strings.Replace(validRolloutBody, `"value": "1"`, `"value": "changed"`, 1)
	recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", conflicting)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if code := errorCode(t, recorder); code != "ConfigConflictError" {
		t.Fatalf("code = %q, want ConfigConflictError", code)
	}

	list := doRequest(t, handler, http.MethodGet, "/v1/config-rollouts?targetEnvironment=prod&namespace=payments&configName=billing", "")
	items, _ := decodeBody(t, list)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 after conflict", len(items))
	}
}

func TestRegisterRolloutRejectsInvalidInput(t *testing.T) {
	handler, _ := newTestRouter(t)

	cases := map[string]string{
		"malformed json":       `{"configName": `,
		"trailing json":        `{} {}`,
		"empty body":           ``,
		"missing configName":   `{"targetEnvironment":"prod","namespace":"payments","version":1,"entries":[{"key":"a","value":"1"}]}`,
		"empty namespace":      `{"configName":"billing","targetEnvironment":"prod","namespace":"","version":1,"entries":[{"key":"a","value":"1"}]}`,
		"missing entries":      `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1}`,
		"empty entries":        `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1,"entries":[]}`,
		"duplicate keys":       `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1,"entries":[{"key":"a","value":"1"},{"key":"a","value":"2"}]}`,
		"empty key":            `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1,"entries":[{"key":"","value":"1"}]}`,
		"empty value":          `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1,"entries":[{"key":"a","value":""}]}`,
		"missing version":      `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","entries":[{"key":"a","value":"1"}]}`,
		"zero version":         `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":0,"entries":[{"key":"a","value":"1"}]}`,
		"negative version":     `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":-2,"entries":[{"key":"a","value":"1"}]}`,
		"fractional version":   `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1.5,"entries":[{"key":"a","value":"1"}]}`,
		"non-numeric version":  `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":"abc","entries":[{"key":"a","value":"1"}]}`,
		"boolean version":      `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":true,"entries":[{"key":"a","value":"1"}]}`,
		"wrong entry type":     `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1,"entries":[{"key":"a","value":7}]}`,
		"entries not an array": `{"configName":"billing","targetEnvironment":"prod","namespace":"payments","version":1,"entries":{"key":"a"}}`,
	}
	for name, body := range cases {
		recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want %d", name, recorder.Code, http.StatusBadRequest)
		}
		if code := errorCode(t, recorder); code != "InvalidConfigInputError" {
			t.Fatalf("%s: code = %q, want InvalidConfigInputError", name, code)
		}
	}
}

func TestListRolloutsReturnsItemsOrderedByVersion(t *testing.T) {
	handler, _ := newTestRouter(t)

	for _, version := range []string{"7", "2", "4"} {
		body := strings.Replace(validRolloutBody, `"version": 2`, `"version": `+version, 1)
		if recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", body); recorder.Code != http.StatusCreated {
			t.Fatalf("seed version %s: status = %d", version, recorder.Code)
		}
	}
	otherNamespace := strings.Replace(validRolloutBody, `"namespace": "payments"`, `"namespace": "other"`, 1)
	if recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", otherNamespace); recorder.Code != http.StatusCreated {
		t.Fatalf("seed other namespace: status = %d", recorder.Code)
	}

	recorder := doRequest(t, handler, http.MethodGet, "/v1/config-rollouts?targetEnvironment=prod&namespace=payments&configName=billing", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	items, _ := decodeBody(t, recorder)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}
	for i, want := range []float64{2, 4, 7} {
		item, _ := items[i].(map[string]any)
		if item["version"] != want {
			t.Fatalf("items[%d].version = %v, want %v", i, item["version"], want)
		}
		if item["configName"] != "billing" || item["digest"] == "" || item["revoked"] != false {
			t.Fatalf("items[%d] missing fields: %v", i, item)
		}
		entries, _ := item["entries"].([]any)
		first, _ := entries[0].(map[string]any)
		if first["key"] != "alpha" {
			t.Fatalf("items[%d] entries not sorted by key: %v", i, item["entries"])
		}
	}
}

func TestListRolloutsRequiresAllQueryParameters(t *testing.T) {
	handler, _ := newTestRouter(t)

	targets := []string{
		"/v1/config-rollouts",
		"/v1/config-rollouts?targetEnvironment=prod&namespace=payments",
		"/v1/config-rollouts?targetEnvironment=prod&configName=billing",
		"/v1/config-rollouts?namespace=payments&configName=billing",
		"/v1/config-rollouts?targetEnvironment=&namespace=payments&configName=billing",
		"/v1/config-rollouts?targetEnvironment=prod&namespace=&configName=billing",
		"/v1/config-rollouts?targetEnvironment=prod&namespace=payments&configName=",
	}
	for _, target := range targets {
		recorder := doRequest(t, handler, http.MethodGet, target, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want %d", target, recorder.Code, http.StatusBadRequest)
		}
		if code := errorCode(t, recorder); code != "InvalidConfigInputError" {
			t.Fatalf("%s: code = %q, want InvalidConfigInputError", target, code)
		}
	}
}

func TestListRolloutsWithoutMatchReturns404(t *testing.T) {
	handler, _ := newTestRouter(t)

	if recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody); recorder.Code != http.StatusCreated {
		t.Fatalf("seed status = %d", recorder.Code)
	}
	recorder := doRequest(t, handler, http.MethodGet, "/v1/config-rollouts?targetEnvironment=staging&namespace=payments&configName=billing", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
	if code := errorCode(t, recorder); code != "ConfigRolloutNotFoundError" {
		t.Fatalf("code = %q, want ConfigRolloutNotFoundError", code)
	}
}

func TestConcurrentIdenticalRegistrationsStayConsistent(t *testing.T) {
	handler, _ := newTestRouter(t)

	var wg sync.WaitGroup
	statuses := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := doRequest(t, handler, http.MethodPost, "/v1/config-rollouts", validRolloutBody)
			statuses <- recorder.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want %d", status, http.StatusCreated)
		}
	}

	list := doRequest(t, handler, http.MethodGet, "/v1/config-rollouts?targetEnvironment=prod&namespace=payments&configName=billing", "")
	items, _ := decodeBody(t, list)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
}
