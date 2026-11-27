package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	shortWindow  = `{"limit_window_seconds":18000,"used_percent":90,"reset_after_seconds":3600}`
	weeklyWindow = `{"limit_window_seconds":604800,"used_percent":20,"reset_after_seconds":172800}`
)

func codexResponse(primary, secondary string) []byte {
	return []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":%s,"secondary_window":%s}}`, primary, secondary))
}

func codexWindow(seconds, remaining, resetSeconds int) string {
	return fmt.Sprintf(`{"limit_window_seconds":%d,"used_percent":%d,"reset_after_seconds":%d}`, seconds, 100-remaining, resetSeconds)
}

func TestCodexWindowBands(t *testing.T) {
	now := time.Now().UTC()
	cfg := defaultSettings()
	cfg.CodexWindowBands = true
	cfg.CodexWeeklyPrimaryMin, cfg.CodexWeeklyRegularMin = 20, 5
	for _, test := range []struct {
		name, short, weekly string
		want                tierName
	}{
		{"short constrains", shortWindow, weeklyWindow, tierBackup},
		{"weekly constrains", codexWindow(18000, 80, 3600), codexWindow(604800, 10, 172800), tierRegular},
		{"short exhausted", codexWindow(18000, 0, 3600), weeklyWindow, tierPaused},
		{"weekly exhausted", shortWindow, codexWindow(604800, 0, 172800), tierPaused},
		{"weekly only", `null`, codexWindow(604800, 30, 172800), tierPrimary},
	} {
		for _, swapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/swapped=%t", test.name, swapped), func(t *testing.T) {
				primary, secondary := test.short, test.weekly
				if swapped {
					primary, secondary = secondary, primary
				}
				quota, err := parseCodexWindowQuota(codexResponse(primary, secondary), now)
				if err != nil {
					t.Fatal(err)
				}
				if weekly := quota.CodexWindows["weekly"]; weekly == nil || !weekly.ResetAt.Equal(now.Add(48*time.Hour)) {
					t.Fatalf("weekly window: %+v", weekly)
				}
				if test.short != `null` {
					if short := quota.CodexWindows["5h"]; short == nil || !short.ResetAt.Equal(now.Add(time.Hour)) {
						t.Fatalf("5-hour window: %+v", short)
					}
				} else if len(quota.CodexWindows) != 1 {
					t.Fatalf("weekly-only response produced %v", quota.CodexWindows)
				}
				got, reason := chooseTier(cfg, authFile{Provider: "codex"}, tierPrimary, quota, now)
				if got != test.want || !strings.Contains(reason, "Weekly:") || strings.Contains(reason, "5-hour:") != (test.short != `null`) {
					t.Fatalf("got %s (%s), want %s", got, reason, test.want)
				}
			})
		}
	}
}

func TestCodexWindowBandsDefaultParity(t *testing.T) {
	now := time.Now().UTC()
	file := authFile{Provider: "codex"}
	legacy, optIn := defaultSettings(), defaultSettings()
	optIn.CodexWindowBands = true
	for _, short := range []int{0, 1, 19, 20, 49, 50, 100} {
		for _, weekly := range []int{0, 1, 19, 20, 49, 50, 100} {
			raw := codexResponse(codexWindow(18000, short, 3600), codexWindow(604800, weekly, 172800))
			quota, err := parseCodexWindowQuota(raw, now)
			if err != nil {
				t.Fatal(err)
			}
			remaining, resetAt, ok := parseCodexQuota(raw, now)
			if !ok {
				t.Fatal("legacy parser rejected the fixture")
			}
			want, _ := chooseTier(legacy, file, tierRegular, readyQuota(remaining, resetAt, now), now)
			if got, _ := chooseTier(optIn, file, tierRegular, quota, now); got != want {
				t.Fatalf("5h %d%%, weekly %d%%: got %s, want %s", short, weekly, got, want)
			}
		}
	}
	quota, err := parseCodexWindowQuota([]byte(`{"rate_limit":{"limit_reached":true,"primary_window":`+weeklyWindow+`}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := chooseTier(optIn, file, tierPrimary, quota, now); got != tierPaused {
		t.Fatalf("top-level exhaustion got %s", got)
	}
	if got, _ := chooseTier(legacy, file, tierPrimary, quota, now); got != tierPrimary {
		t.Fatalf("opt-in exhaustion flag leaked into legacy aggregate: %s", got)
	}
	optIn.CodexWeeklyPrimaryMin, optIn.CodexWeeklyRegularMin = 10, 5
	if got, _ := chooseTier(optIn, authFile{Provider: "antigravity"}, tierPrimary, readyQuota(30, nil, now), now); got != tierRegular {
		t.Fatalf("Codex thresholds changed Antigravity tier: %s", got)
	}
}

func TestCodexWindowBandsRejectMalformedResponse(t *testing.T) {
	for _, test := range []struct{ name, primary, secondary string }{
		{"no windows", `null`, `null`},
		{"missing duration", `{"used_percent":20,"reset_after_seconds":100}`, weeklyWindow},
		{"unknown duration", strings.Replace(shortWindow, "18000", "100", 1), weeklyWindow},
		{"duplicate duration", weeklyWindow, weeklyWindow},
		{"invalid window", `[]`, weeklyWindow},
		{"missing reset", `{"limit_window_seconds":18000,"used_percent":20}`, weeklyWindow},
		{"invalid reset", strings.Replace(shortWindow, `:3600`, `:1e30`, 1), weeklyWindow},
		{"invalid quota", strings.Replace(shortWindow, `:90`, `:-1`, 1), weeklyWindow},
		{"nonfinite quota", strings.Replace(shortWindow, `:90`, `:"NaN"`, 1), weeklyWindow},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseCodexWindowQuota(codexResponse(test.primary, test.secondary), time.Now()); err == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
}

func newCodexTestRuntime(t *testing.T, cfg settings) (*runtime, *fakeHost) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("CREDENTIAL_TIER_ROUTER_STATE_PATH", path)
	host := &fakeHost{
		files:     []authFile{{Name: "a.json", AuthIndex: "a", Provider: "codex", Priority: 200}},
		documents: map[string]authDocument{"a": {Name: "a.json", JSON: json.RawMessage(`{}`)}},
		responses: map[string]hostHTTPResponse{},
	}
	r := newRuntime(host)
	r.state.Settings = cfg
	return r, host
}

func testCodexProbe(t *testing.T, r *runtime, host *fakeHost, body []byte) credentialState {
	t.Helper()
	host.responses["a"] = hostHTTPResponse{StatusCode: http.StatusOK, Body: body}
	result, err := r.run(context.Background(), false, "test")
	if err != nil || len(result.Credentials) != 1 {
		t.Fatalf("probe failed: %+v %v", result, err)
	}
	return result.Credentials[0]
}

func TestCodexWindowLossUsesProbeFailureGuardrail(t *testing.T) {
	cfg := defaultSettings()
	cfg.CodexWindowBands = true
	r, host := newCodexTestRuntime(t, cfg)
	initial := testCodexProbe(t, r, host, codexResponse(shortWindow, weeklyWindow))
	if initial.Quota.Status != quotaReady || initial.ProposedTier != tierBackup {
		t.Fatalf("initial probe: %+v", initial)
	}
	for i, want := range []quotaStatus{quotaCached, quotaCached, quotaUnknown, quotaUnknown} {
		if want == quotaUnknown {
			r = newRuntime(host)
		}
		state := testCodexProbe(t, r, host, codexResponse(weeklyWindow, `null`))
		if state.Quota.Status != want || state.Quota.FailCount != i+1 || state.ProposedTier != tierBackup || state.Quota.LastError != quotaCheckFailed {
			t.Fatalf("probe failure %d: %+v", i+1, state)
		}
		if want == quotaUnknown {
			window, seen := state.Quota.CodexWindows["5h"]
			if !seen || window != nil || state.Quota.Remaining != nil {
				t.Fatalf("unknown quota should keep only the shape: %+v", state.Quota)
			}
		}
	}
	restored := testCodexProbe(t, r, host, codexResponse(weeklyWindow, codexWindow(18000, 80, 3600)))
	if restored.ProposedTier != tierPrimary || restored.Quota.Status != quotaReady || restored.Quota.FailCount != 0 {
		t.Fatalf("restored windows: %+v", restored)
	}
}

func TestCodexLegacyStrategiesKeepProbeAndResetBehavior(t *testing.T) {
	for _, tc := range []struct {
		strategy strategyName
		optIn    bool
		want     tierName
	}{
		{strategyQuota, false, tierPrimary}, {strategyReset, true, tierPrimary},
		{strategyRotate, true, tierRegular}, {strategyManual, true, tierRegular},
	} {
		t.Run(string(tc.strategy), func(t *testing.T) {
			cfg := defaultSettings()
			cfg.Strategy, cfg.CodexWindowBands = tc.strategy, tc.optIn
			cfg.ManualTiers["a"] = tierRegular
			r, host := newCodexTestRuntime(t, cfg)
			// An absent duration and an unknown duration are both legal in legacy mode.
			// The root limit flag is ignored, and a tie retains the primary reset time.
			raw := []byte(`{"rate_limit":{"limit_reached":true,"primary_window":{"remaining":80,"reset_after_seconds":3600},"secondary_window":{"used_percent":20,"limit_window_seconds":42,"reset_after_seconds":172800}}}`)
			state := testCodexProbe(t, r, host, raw)
			q := state.Quota
			if q.Status != quotaReady || q.Remaining == nil || *q.Remaining != 80 || q.ResetAt == nil || q.ResetAt.Sub(q.ObservedAt) != time.Hour || len(q.CodexWindows) != 0 || state.ProposedTier != tc.want {
				t.Fatalf("legacy response: %+v", state)
			}
			for _, want := range []quotaStatus{quotaCached, quotaCached, quotaUnknown} {
				state = testCodexProbe(t, r, host, []byte(`{}`))
				tier := tc.want
				if want == quotaUnknown {
					tier = tierBackup
				}
				if state.Quota.Status != want || state.ProposedTier != tier {
					t.Fatalf("legacy failure: %+v", state)
				}
			}
			if tc.strategy == strategyReset {
				state = testCodexProbe(t, r, host, codexResponse(`{"used_percent":20,"reset_after_seconds":3600}`, `{"used_percent":21,"reset_after_seconds":172800}`))
				if state.Quota.Status != quotaReady || state.ProposedTier != tierRegular {
					t.Fatalf("reset_soon should follow the most restrictive window's later reset: %+v", state)
				}
			}
			// Skip the malformed sibling and apply exhaustion before strategy routing.
			state = testCodexProbe(t, r, host, codexResponse(`[]`, `{"used_percent":100,"reset_after_seconds":172800}`))
			if state.Quota.Status != quotaReady || state.ProposedTier != tierPaused || state.Quota.FailCount != 0 {
				t.Fatalf("legacy exhaustion: %+v", state)
			}
		})
	}
}

func TestCodexBandsSettingsMigration(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("codex_window_bands: true\ncodex_weekly_primary_min: 20\ncodex_weekly_regular_min: 5"))
	if err != nil || !cfg.CodexWindowBands || cfg.CodexWeeklyPrimaryMin != 20 || cfg.CodexWeeklyRegularMin != 5 || cfg.Codex5hPrimaryMin != 50 || cfg.Codex5hRegularMin != 20 {
		t.Fatalf("host config: %+v %v", cfg, err)
	}
	for _, invalid := range []string{`{"codex_weekly_regular_min":0}`, `{"codex_weekly_regular_min":50}`, `{"codex_5h_primary_min":101}`, "codex_5h_primary_min: invalid", "codex_window_bands: invalid"} {
		if _, err := parsePluginConfig([]byte(invalid)); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}

	legacy := `{"strategy":"reset_soon","auto_apply":false,"interval_minutes":15,"failure_threshold":3,"providers":["codex"],"antigravity_group":"gemini"}`
	store := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("CREDENTIAL_TIER_ROUTER_STATE_PATH", store)
	if err := os.WriteFile(store, []byte(`{"settings":`+legacy+`,"quota":{"a":{"remaining":80,"status":"ready"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRuntime(&fakeHost{})
	if r.state.Settings.Strategy != strategyReset || r.state.Settings.codexBandSettings != defaultCodexBandSettings() || r.state.Quota["a"].Remaining == nil {
		t.Fatalf("legacy state did not migrate: %+v", r.state)
	}
	if got, _ := chooseTier(cfg, authFile{Provider: "codex"}, tierBackup, r.state.Quota["a"], time.Now()); got != tierBackup {
		t.Fatalf("opt-in routed a legacy aggregate as a known window: %s", got)
	}
	defer r.shutdown()
	for _, body := range []string{legacy, `{"strategy":"quota_bands","interval_minutes":15,"failure_threshold":3,"providers":["codex"],"antigravity_group":"gemini","codex_window_bands":true,"codex_weekly_regular_min":5,"codex_weekly_primary_min":20}`} {
		request, err := json.Marshal(map[string]any{"Method": "PUT", "Path": "/plugins/credential-tier-router/settings", "Body": []byte(body)})
		if err != nil {
			t.Fatal(err)
		}
		response, err := r.handleManagement(context.Background(), request)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("save settings: %s %v", response.Body, err)
		}
		want := defaultCodexBandSettings()
		if strings.Contains(body, "codex_window_bands") {
			want.CodexWindowBands, want.CodexWeeklyPrimaryMin, want.CodexWeeklyRegularMin = true, 20, 5
		}
		saved, err := r.store.load()
		if err != nil || saved.Settings.codexBandSettings != want {
			t.Fatalf("settings did not round-trip: %+v %v", saved.Settings, err)
		}
	}
}
