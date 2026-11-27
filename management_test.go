package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const managementTestPath = "/v0/management/plugins/" + pluginID
const syntheticSecret = "synthetic-sensitive-detail"

type managementErrorHost struct {
	fakeHost
	panicOnList bool
	err         error
}

func (h *managementErrorHost) listAuth(context.Context) ([]authFile, error) {
	if h.panicOnList {
		panic(syntheticSecret)
	}
	return h.fakeHost.files, h.err
}

func (h *managementErrorHost) getAuth(context.Context, string) (authDocument, error) {
	return authDocument{}, errors.New("Bearer " + syntheticSecret)
}

func managementResult(t *testing.T, raw []byte) managementResponse {
	t.Helper()
	var envelope struct {
		OK     bool               `json:"ok"`
		Result managementResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || !envelope.OK {
		t.Fatalf("expected a management HTTP response in a successful plugin envelope, got %s (%v)", raw, err)
	}
	return envelope.Result
}

func managementCall(t *testing.T, r *runtime, method, path string, body []byte) managementResponse {
	t.Helper()
	raw, err := json.Marshal(struct {
		Method, Path string
		Body         []byte
	}{method, managementTestPath + path, body})
	if err != nil {
		t.Fatal(err)
	}
	return managementResult(t, r.handle(t.Context(), "management.handle", raw))
}

func checkManagementError(t *testing.T, response managementResponse, status int, message string) {
	t.Helper()
	if response.StatusCode != status || strings.Join(response.Headers["Content-Type"], ",") != "application/json; charset=utf-8" || strings.Join(response.Headers["Cache-Control"], ",") != "no-store" {
		t.Fatalf("unexpected HTTP response: %+v", response)
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body, &payload); err != nil || len(payload) != 1 || payload["error"] != message {
		t.Fatalf("expected only the public error %q, got %s (%v)", message, response.Body, err)
	}
}

func TestManagementFailuresRemainJSON(t *testing.T) {
	t.Setenv("CREDENTIAL_TIER_ROUTER_STATE_PATH", filepath.Join(t.TempDir(), "state.json"))
	for name, raw := range map[string]string{
		"malformed envelope": "{",
		"invalid base64":     `{"Method":"PUT","Path":"/settings","Body":"***"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRuntime(&fakeHost{})
			response := managementResult(t, r.handle(t.Context(), "management.handle", []byte(raw)))
			checkManagementError(t, response, http.StatusInternalServerError, "Management request failed")
		})
	}
	t.Run("panic", func(t *testing.T) {
		r := newRuntime(&managementErrorHost{panicOnList: true})
		response := managementCall(t, r, http.MethodPost, "/preview", nil)
		checkManagementError(t, response, http.StatusInternalServerError, "Management request failed")
		if !r.runMu.TryLock() {
			t.Fatal("preview panic left the run lock held")
		}
		r.runMu.Unlock()
	})
	t.Run("host failure", func(t *testing.T) {
		r := newRuntime(&managementErrorHost{err: errors.New("Bearer " + syntheticSecret)})
		checkManagementError(t, managementCall(t, r, http.MethodGet, "/state", nil), http.StatusInternalServerError, "Could not load credential status")
		checkManagementError(t, managementCall(t, r, http.MethodPost, "/preview", nil), http.StatusInternalServerError, "Could not preview tier changes")
		checkManagementError(t, managementCall(t, r, http.MethodPost, "/apply", nil), http.StatusInternalServerError, "Could not apply tier changes")
	})
	t.Run("settings validation", func(t *testing.T) {
		r := newRuntime(&fakeHost{})
		checkManagementError(t, managementCall(t, r, http.MethodPut, "/settings", []byte(`{"strategy":"`+syntheticSecret+`"}`)), http.StatusBadRequest, "Invalid settings values")
	})
}

func TestManagementDoesNotExposeProbeOrApplyErrors(t *testing.T) {
	t.Setenv("CREDENTIAL_TIER_ROUTER_STATE_PATH", filepath.Join(t.TempDir(), "state.json"))
	h := &managementErrorHost{fakeHost: fakeHost{files: []authFile{{Name: "fixture.json", Provider: "codex", AuthIndex: "fixture", Priority: 100}}}}
	r := newRuntime(h)
	r.state.Settings.Strategy = strategyManual
	r.state.Settings.ManualTiers["fixture"] = tierBackup
	r.state.History = []historyEntry{{Summary: "Apply failed: " + syntheticSecret}}
	quota := readyQuota(75, nil, time.Now())
	quota.LastError = syntheticSecret
	r.state.Quota["fixture"] = quota
	for _, step := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/state", http.StatusOK},
		{http.MethodPost, "/preview", http.StatusOK},
		{http.MethodPost, "/apply", http.StatusInternalServerError},
		{http.MethodGet, "/state", http.StatusOK},
	} {
		response := managementCall(t, r, step.method, step.path, nil)
		if response.StatusCode != step.status || strings.Contains(string(response.Body), syntheticSecret) {
			t.Fatalf("unexpected %s response: HTTP %d %s", step.path, response.StatusCode, response.Body)
		}
	}
}

func TestManagementPreviewIsPostOnly(t *testing.T) {
	raw, err := json.Marshal(managementRegistration())
	if err != nil {
		t.Fatal(err)
	}
	var registration struct {
		Routes []struct{ Method, Path string }
	}
	if err := json.Unmarshal(raw, &registration); err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, route := range registration.Routes {
		if route.Path == "/plugins/"+pluginID+"/preview" {
			methods = append(methods, route.Method)
		}
	}
	if len(methods) != 1 || methods[0] != http.MethodPost {
		t.Fatalf("registered preview methods = %v, want POST only", methods)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("CREDENTIAL_TIER_ROUTER_STATE_PATH", path)
	host := &fakeHost{
		files:     []authFile{{Name: "fixture.json", Provider: "codex", AuthIndex: "fixture"}},
		documents: map[string]authDocument{"fixture": {JSON: json.RawMessage(`{"access_token":"synthetic-token"}`)}},
		responses: map[string]hostHTTPResponse{"fixture": {StatusCode: http.StatusOK, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":12,"reset_after_seconds":3600}}}`)}},
	}
	r := newRuntime(host)
	checkManagementError(t, managementCall(t, r, http.MethodGet, "/preview", nil), http.StatusNotFound, "route not found")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || len(r.state.Quota) != 0 {
		t.Fatalf("GET /preview modified quota state: %v", err)
	}
	response := managementCall(t, r, http.MethodPost, "/preview", nil)
	var result plan
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &result) != nil || result.Changes != 1 {
		t.Fatalf("expected one proposed change, got HTTP %d %s", response.StatusCode, response.Body)
	}
	persisted, err := r.store.load()
	if err != nil || len(persisted.History) != 0 || len(host.saved) != 0 || r.latest.Changes != 1 {
		t.Fatalf("preview should update the latest plan without writing credentials or history: %v", err)
	}
	quota := persisted.Quota["fixture"]
	if quota.Remaining == nil || *quota.Remaining != 88 {
		t.Fatalf("expected preview to persist refreshed quota: %+v", quota)
	}
	r.runMu.Lock()
	defer r.runMu.Unlock()
	checkManagementError(t, managementCall(t, r, http.MethodPost, "/preview", nil), http.StatusConflict, "A quota check is already in progress")
}

func TestManagementSettingsSaveErrorIsJSON(t *testing.T) {
	file := filepath.Join(t.TempDir(), syntheticSecret)
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIAL_TIER_ROUTER_STATE_PATH", filepath.Join(file, "state.json"))
	r := newRuntime(&fakeHost{})
	next := defaultSettings()
	next.AutoApply = true
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	checkManagementError(t, managementCall(t, r, http.MethodPut, "/settings", body), http.StatusInternalServerError, "Could not save settings")
	if r.state.Settings.AutoApply || r.cancel != nil {
		t.Fatal("a failed save activated the requested settings")
	}
}
