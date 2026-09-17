package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeHost struct {
	files     []authFile
	documents map[string]authDocument
	responses map[string]hostHTTPResponse
	saved     map[string]json.RawMessage
}

func (f *fakeHost) listAuth(context.Context) ([]authFile, error) {
	return append([]authFile(nil), f.files...), nil
}

func (f *fakeHost) getAuth(_ context.Context, index string) (authDocument, error) {
	document, ok := f.documents[index]
	if !ok {
		return authDocument{}, errors.New("missing auth")
	}
	return document, nil
}

func (f *fakeHost) saveAuth(_ context.Context, name string, document json.RawMessage) error {
	if f.saved == nil {
		f.saved = map[string]json.RawMessage{}
	}
	f.saved[name] = append(json.RawMessage(nil), document...)
	return nil
}

func (f *fakeHost) httpDo(_ context.Context, request hostHTTPRequest) (hostHTTPResponse, error) {
	response, ok := f.responses[request.AuthIndex]
	if !ok {
		return hostHTTPResponse{}, errors.New("probe unavailable")
	}
	return response, nil
}

func TestQuotaBandTiers(t *testing.T) {
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		remaining int
		want      tierName
	}{
		{100, tierPrimary}, {50, tierPrimary}, {49, tierRegular}, {20, tierRegular}, {19, tierBackup}, {1, tierBackup}, {0, tierPaused},
	} {
		got, _ := chooseTier(defaultSettings(), authFile{}, tierRegular, readyQuota(test.remaining, ptrTime(now.Add(time.Hour)), now), now)
		if got != test.want {
			t.Fatalf("remaining %d: got %s, want %s", test.remaining, got, test.want)
		}
	}
}

func TestStrategies(t *testing.T) {
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	quota := readyQuota(10, ptrTime(now.Add(12*time.Hour)), now)
	cfg := defaultSettings()
	cfg.Strategy = strategyRotate
	if got, _ := chooseTier(cfg, authFile{}, tierBackup, quota, now); got != tierRegular {
		t.Fatalf("balanced got %s", got)
	}
	cfg.Strategy = strategyReset
	if got, _ := chooseTier(cfg, authFile{}, tierBackup, quota, now); got != tierPrimary {
		t.Fatalf("reset soon got %s", got)
	}
	cfg.Strategy = strategyManual
	cfg.ManualTiers["auth-1"] = tierBackup
	if got, _ := chooseTier(cfg, authFile{AuthIndex: "auth-1"}, tierPrimary, quota, now); got != tierBackup {
		t.Fatalf("manual got %s", got)
	}
}

func TestFailedQuotaKeepsLastResultThenUnknown(t *testing.T) {
	now := time.Now().UTC()
	previous := readyQuota(62, ptrTime(now.Add(time.Hour)), now)
	first := failedQuota(previous, errors.New("temporary"), 3, now.Add(time.Minute))
	if first.Status != quotaCached || first.Remaining == nil || *first.Remaining != 62 {
		t.Fatalf("first failure did not retain last quota: %+v", first)
	}
	second := failedQuota(first, errors.New("temporary"), 3, now.Add(2*time.Minute))
	if second.Status != quotaCached {
		t.Fatalf("second failure status=%s", second.Status)
	}
	third := failedQuota(second, errors.New("temporary"), 3, now.Add(3*time.Minute))
	if third.Status != quotaUnknown || third.Remaining != nil {
		t.Fatalf("third failure should be unknown: %+v", third)
	}
}

func TestApplyPreservesCredentialAndUsesSoftPause(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{
		documents: map[string]authDocument{"a": {AuthIndex: "a", Name: "account.json", JSON: json.RawMessage(`{"access_token":"secret","email":"user@example.com","custom":"keep","disabled":false}`)}},
	}
	r := newRuntime(host)
	r.store.path = filepath.Join(dir, "state.json")
	result := plan{Credentials: []credentialState{{AuthIndex: "a", ProposedTier: tierPaused, Changed: true}}}
	if err := r.applyPlan(context.Background(), []authFile{{AuthIndex: "a", Name: "account.json"}}, result); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(host.saved["account.json"], &saved); err != nil {
		t.Fatal(err)
	}
	if saved["custom"] != "keep" || saved["access_token"] != "secret" {
		t.Fatalf("unrelated fields not preserved: %v", saved)
	}
	if saved["disabled"] != false || int(saved["priority"].(float64)) != -1 {
		t.Fatalf("paused must use priority -1 without hard disable: %v", saved)
	}
}

func TestManagementResourceContainsNoCredentialData(t *testing.T) {
	r := newRuntime(&fakeHost{})
	request, _ := json.Marshal(map[string]any{"Method": "GET", "Path": "/v0/resource/plugins/credential-tier-router/status"})
	response, err := r.handleManagement(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || len(response.Body) < 1000 {
		t.Fatalf("unexpected resource response: %d %d", response.StatusCode, len(response.Body))
	}
	if json.Valid(response.Body) {
		t.Fatal("resource should be static HTML, not a dynamic JSON payload")
	}
	html := string(response.Body)
	for _, forbidden := range []string{`id="managementKey"`, `id="keyDialog"`, `type="password"`, "localStorage.setItem", "sessionStorage.setItem"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("management resource contains forbidden login UI or storage write: %s", forbidden)
		}
	}
	for _, required := range []string{"cli-proxy-auth", "connectionNotice"} {
		if !strings.Contains(html, required) {
			t.Fatalf("management resource missing CPA sign-in reuse marker: %s", required)
		}
	}
}

func TestPreviewReturnsConflictWhileQuotaCheckRuns(t *testing.T) {
	r := newRuntime(&fakeHost{})
	r.runMu.Lock()
	defer r.runMu.Unlock()
	request, _ := json.Marshal(map[string]any{"Method": "POST", "Path": "/v0/management/plugins/credential-tier-router/preview"})
	response, err := r.handleManagement(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 409 {
		t.Fatalf("got status %d, want 409", response.StatusCode)
	}
}

func TestManagedAuthFileExcludesBackupPaths(t *testing.T) {
	if managedAuthFile(authFile{Name: "backups/old/codex-account.json", Provider: "codex"}) {
		t.Fatal("backup subdirectory entry must not be managed")
	}
	if !managedAuthFile(authFile{Name: "codex-account.json", Provider: "codex"}) {
		t.Fatal("active auth file should be managed")
	}
}

func TestStateStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store := stateStore{path: path}
	state := persistedState{Settings: defaultSettings(), Quota: map[string]quotaSnapshot{}, History: []historyEntry{{Summary: "ok"}}}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%o", info.Mode().Perm())
	}
	loaded, err := store.load()
	if err != nil || len(loaded.History) != 1 {
		t.Fatalf("round trip failed: %+v %v", loaded, err)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
