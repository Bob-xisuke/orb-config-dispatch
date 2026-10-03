package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Bob-xisuke/orb-config-dispatch/internal/store"
)

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type registerResponse struct {
	ConfigName        string `json:"configName"`
	TargetEnvironment string `json:"targetEnvironment"`
	Namespace         string `json:"namespace"`
	Version           int64  `json:"version"`
	Entries           []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"entries"`
	Digest  string `json:"digest"`
	Revoked bool   `json:"revoked"`
}

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st)
}

func postRollout(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/config-rollouts", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func validBody() string {
	return `{"configName":"payments","targetEnvironment":"staging","namespace":"billing","version":1,"entries":[{"key":"b","value":"2"},{"key":"a","value":"1"}]}`
}

func assertAPIError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, status, recorder.Body.String())
	}
	var body errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, recorder.Body.String())
	}
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q with message", body.Error, code)
	}
	if strings.ContainsAny(body.Error.Message, "\n\r") {
		t.Fatal("error message must stay generic")
	}
}

func TestRegisterRolloutCreated(t *testing.T) {
	router := newTestRouter(t)
	recorder := postRollout(t, router, validBody())

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body registerResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ConfigName != "payments" || body.TargetEnvironment != "staging" ||
		body.Namespace != "billing" || body.Version != 1 {
		t.Fatalf("identity fields mismatch: %+v", body)
	}
	if body.Revoked {
		t.Fatal("revoked must start false")
	}
	if len(body.Entries) != 2 || body.Entries[0].Key != "a" || body.Entries[1].Key != "b" {
		t.Fatalf("entries must be sorted by key: %+v", body.Entries)
	}
	if len(body.Digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex chars", body.Digest)
	}
}

func TestRegisterRolloutOrderIndependentDigest(t *testing.T) {
	router := newTestRouter(t)
	first := postRollout(t, router, validBody())
	reordered := `{"configName":"payments","targetEnvironment":"staging","namespace":"billing","version":1,"entries":[{"key":"a","value":"1"},{"key":"b","value":"2"}]}`
	second := postRollout(t, router, reordered)

	if second.Code != http.StatusCreated {
		t.Fatalf("duplicate status = %d: %s", second.Code, second.Body.String())
	}
	var a, b registerResponse
	json.Unmarshal(first.Body.Bytes(), &a)
	json.Unmarshal(second.Body.Bytes(), &b)
	if a.Digest != b.Digest {
		t.Fatalf("same content must share digest: %q vs %q", a.Digest, b.Digest)
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("idempotent submissions must return the same record:\n%s\n%s", first.Body.String(), second.Body.String())
	}

	changed := `{"configName":"payments","targetEnvironment":"staging","namespace":"billing","version":1,"entries":[{"key":"a","value":"1"},{"key":"b","value":"9"}]}`
	assertAPIError(t, postRollout(t, router, changed), http.StatusConflict, "ConfigConflictError")
}

func TestRegisterValidationErrors(t *testing.T) {
	router := newTestRouter(t)
	good := func() map[string]any {
		return map[string]any{
			"configName": "payments", "targetEnvironment": "staging", "namespace": "billing",
			"version": 1, "entries": []map[string]string{{"key": "a", "value": "1"}},
		}
	}

	cases := map[string]func(map[string]any){
		"malformed json":     func(m map[string]any) {},
		"missing configName": func(m map[string]any) { m["configName"] = "" },
		"blank environment":  func(m map[string]any) { m["targetEnvironment"] = "   " },
		"missing namespace":  func(m map[string]any) { delete(m, "namespace") },
		"zero version":       func(m map[string]any) { m["version"] = 0 },
		"negative version":   func(m map[string]any) { m["version"] = -3 },
		"decimal version":    func(m map[string]any) { m["version"] = 1.5 },
		"string version":     func(m map[string]any) { m["version"] = "1" },
		"null version":       func(m map[string]any) { m["version"] = nil },
		"missing entries":    func(m map[string]any) { delete(m, "entries") },
		"empty entries":      func(m map[string]any) { m["entries"] = []any{} },
		"blank key":          func(m map[string]any) { m["entries"] = []map[string]string{{"key": "", "value": "1"}} },
		"blank value":        func(m map[string]any) { m["entries"] = []map[string]string{{"key": "a", "value": " "}} },
		"duplicate keys": func(m map[string]any) {
			m["entries"] = []map[string]string{{"key": "a", "value": "1"}, {"key": "a", "value": "2"}}
		},
		"entries not an array": func(m map[string]any) { m["entries"] = map[string]string{"key": "a", "value": "1"} },
		"body not an object":   func(m map[string]any) {},
		"trailing json":        func(m map[string]any) {},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var body string
			switch name {
			case "malformed json":
				body = `{"configName":"x",`
			case "body not an object":
				body = `[1,2,3]`
			case "trailing json":
				raw, _ := json.Marshal(good())
				body = string(raw) + ` {"extra":true}`
			default:
				payload := good()
				mutate(payload)
				raw, _ := json.Marshal(payload)
				body = string(raw)
			}
			assertAPIError(t, postRollout(t, router, body), http.StatusBadRequest, "InvalidConfigInputError")
		})
	}
}

func TestListRolloutsOrderingAndErrors(t *testing.T) {
	router := newTestRouter(t)

	withVersion := func(version int) string {
		return strings.Replace(validBody(), `"version":1`, fmt.Sprintf(`"version":%d`, version), 1)
	}
	for _, version := range []int{3, 1, 2} {
		postRollout(t, router, withVersion(version))
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		"/v1/config-rollouts?targetEnvironment=staging&namespace=billing&configName=payments", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []registerResponse `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(body.Items))
	}
	for index, want := range []int64{1, 2, 3} {
		if body.Items[index].Version != want {
			t.Fatalf("item %d version = %d, want %d", index, body.Items[index].Version, want)
		}
	}

	for _, query := range []string{
		"/v1/config-rollouts?targetEnvironment=staging&namespace=billing",
		"/v1/config-rollouts?targetEnvironment=&namespace=billing&configName=payments",
		"/v1/config-rollouts?targetEnvironment=staging&namespace=billing&configName=payments&extra=1",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, query, nil))
		assertAPIError(t, recorder, http.StatusBadRequest, "InvalidConfigInputError")
	}

	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/v1/config-rollouts?targetEnvironment=prod&namespace=billing&configName=payments", nil))
	assertAPIError(t, recorder, http.StatusNotFound, "ConfigRolloutNotFoundError")
}

func TestConcurrentIdenticalPostCreatesOneRecord(t *testing.T) {
	router := newTestRouter(t)
	const goroutines = 10
	var wg sync.WaitGroup
	statuses := make(chan int, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/config-rollouts", strings.NewReader(validBody()))
			router.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		}()
	}
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
	}
}
