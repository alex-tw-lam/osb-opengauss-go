package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

var errPingFailed = errors.New("connection refused")

// jsonReader turns a JSON string into a request body.
func jsonReader(body string) io.Reader { return strings.NewReader(body) }

// withAuth adds the broker basic auth and API version headers.
func withAuth(request *http.Request) *http.Request {
	request.SetBasicAuth("broker", "x")
	request.Header.Set("X-Broker-API-Version", "2.16")
	return request
}

// newTestServer builds the real HTTP handler on a fake database.
func newTestServer(t *testing.T, db *fakeDB) http.Handler {
	t.Helper()
	cfg := testConfig("role_quota")
	cfg.StatePath = filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	broker := NewBroker(cfg, []Plan{devPlan}, NewAdmin(cfg, db), store, slog.Default())
	return newHandler(cfg, broker, slog.Default())
}

func TestCatalogOverHTTP(t *testing.T) {
	server := newTestServer(t, newFakeDB())

	noAuth := httptest.NewRequest(http.MethodGet, "/v2/catalog", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, noAuth)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("without auth = %d, want 401", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/v2/catalog", nil)
	request.SetBasicAuth("broker", "x")
	request.Header.Set("X-Broker-API-Version", "2.16")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("catalog = %d: %s", recorder.Code, recorder.Body)
	}
	var catalog struct {
		Services []struct {
			Name  string `json:"name"`
			Plans []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"plans"`
		} `json:"services"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Services) != 1 || catalog.Services[0].Name != "gaussdb" || len(catalog.Services[0].Plans) != 1 {
		t.Fatalf("catalog wrong: %+v", catalog)
	}
}

func TestProvisionOverHTTP(t *testing.T) {
	server := newTestServer(t, newFakeDB())
	body := `{"service_id":"` + serviceID + `","plan_id":"gaussdb-dev"}`
	request := httptest.NewRequest(http.MethodPut, "/v2/service_instances/"+iid+"?accepts_incomplete=false", jsonReader(body))
	request.SetBasicAuth("broker", "x")
	request.Header.Set("X-Broker-API-Version", "2.16")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("provision = %d: %s", recorder.Code, recorder.Body)
	}

	// Identical repeat is 200, conflict is 409.
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, withAuth(httptest.NewRequest(http.MethodPut, "/v2/service_instances/"+iid+"?accepts_incomplete=false", jsonReader(body))))
	if recorder.Code != http.StatusOK {
		t.Fatalf("identical re-provision = %d, want 200", recorder.Code)
	}
	conflict := `{"service_id":"` + serviceID + `","plan_id":"gaussdb-dev","parameters":{"max_connections":5}}`
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, withAuth(httptest.NewRequest(http.MethodPut, "/v2/service_instances/"+iid+"?accepts_incomplete=false", jsonReader(conflict))))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("conflicting re-provision = %d, want 409", recorder.Code)
	}

	// Unknown plan is 400.
	bad := `{"service_id":"` + serviceID + `","plan_id":"nope"}`
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, withAuth(httptest.NewRequest(http.MethodPut, "/v2/service_instances/99999999-9999-9999-9999-999999999999?accepts_incomplete=false", jsonReader(bad))))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown plan = %d, want 400", recorder.Code)
	}
}

func TestHealthzOverHTTP(t *testing.T) {
	server := newTestServer(t, newFakeDB())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ok"`) {
		t.Fatalf("healthz = %d: %s", recorder.Code, recorder.Body)
	}

	broken := newFakeDB()
	broken.pingErr = errPingFailed
	recorder = httptest.NewRecorder()
	newTestServer(t, broken).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with broken database = %d, want 503", recorder.Code)
	}
}
