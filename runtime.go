package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type runtime struct {
	host hostAPI

	mu        sync.Mutex
	runMu     sync.Mutex
	state     persistedState
	latest    plan
	store     stateStore
	cancel    context.CancelFunc
	done      chan struct{}
	nextProbe *time.Time
	closed    bool
}

func newRuntime(host hostAPI) *runtime {
	statePath := os.Getenv("CREDENTIAL_TIER_ROUTER_STATE_PATH")
	if statePath == "" {
		statePath = filepath.Join("credential-tier-router", "state.json")
	}
	r := &runtime{host: host, store: stateStore{path: statePath}, done: make(chan struct{})}
	if state, err := r.store.load(); err == nil {
		r.state = state
	} else {
		r.state = persistedState{Settings: defaultSettings(), Quota: map[string]quotaSnapshot{}}
	}
	return r
}

func (r *runtime) handle(ctx context.Context, method string, request []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		cfg, err := decodeLifecycleConfig(request)
		if err != nil {
			return failure("invalid_config", err.Error(), false)
		}
		if err := r.configure(cfg); err != nil {
			return failure("invalid_config", err.Error(), false)
		}
		return success(registrationResult())
	case "plugin.shutdown":
		r.shutdown()
		return success(map[string]string{"status": "ok"})
	case "management.register":
		return success(managementRegistration())
	case "management.handle":
		response, err := r.handleManagement(ctx, request)
		if err != nil {
			return failure("invalid_request", err.Error(), false)
		}
		return success(response)
	default:
		return failure("invalid_request", fmt.Sprintf("unsupported method %q", method), false)
	}
}

func decodeLifecycleConfig(raw []byte) (settings, error) {
	if len(raw) == 0 {
		return defaultSettings(), nil
	}
	var wire struct {
		ConfigYAML json.RawMessage `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return settings{}, err
	}
	if len(wire.ConfigYAML) == 0 || string(wire.ConfigYAML) == "null" {
		return defaultSettings(), nil
	}
	var encoded string
	if err := json.Unmarshal(wire.ConfigYAML, &encoded); err != nil {
		return settings{}, err
	}
	configBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		configBytes = []byte(encoded)
	}
	return parsePluginConfig(configBytes)
}

func (r *runtime) configure(config settings) error {
	if err := config.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("plugin is shut down")
	}
	// Settings saved from the plugin page are the durable source of truth. Host
	// config seeds a new installation but does not erase page changes on reload.
	if _, err := os.Stat(r.store.path); errors.Is(err, os.ErrNotExist) {
		r.state.Settings = config
	}
	if r.state.Quota == nil {
		r.state.Quota = map[string]quotaSnapshot{}
	}
	state := r.state
	r.mu.Unlock()
	if err := r.store.save(state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	r.restartWorker()
	return nil
}

func (r *runtime) restartWorker() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	done := r.done
	cfg := r.state.Settings
	r.mu.Unlock()
	go func() {
		defer close(done)
		if !cfg.AutoApply {
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(cfg.interval())
		defer timer.Stop()
		for {
			next := time.Now().UTC().Add(cfg.interval())
			r.mu.Lock()
			r.nextProbe = &next
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				_, _ = r.run(context.Background(), true, "Automatic scheduling")
				timer.Reset(cfg.interval())
			}
		}
	}()
}

func (r *runtime) shutdown() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	cancel := r.cancel
	done := r.done
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

func registrationResult() map[string]any {
	return map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             "Credential Tiers",
			"Version":          pluginVersion,
			"Author":           "William-zgx",
			"GitHubRepository": "https://github.com/William-zgx/cpa-plugin-credential-tier-router",
			"Description":      "Routes Codex and Antigravity credentials through clear primary, regular, backup, and paused tiers.",
			"ConfigFields":     []any{},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []map[string]string{
			{"Method": "GET", "Path": "/plugins/" + pluginID + "/state"},
			{"Method": "POST", "Path": "/plugins/" + pluginID + "/preview"},
			{"Method": "POST", "Path": "/plugins/" + pluginID + "/apply"},
			{"Method": "PUT", "Path": "/plugins/" + pluginID + "/settings"},
		},
		"resources": []map[string]string{{"Path": "/status", "Menu": "Credential Tiers", "Description": "Manage Codex and Antigravity credential tiers."}},
	}
}

func (r *runtime) dashboard(ctx context.Context) (dashboardState, error) {
	r.mu.Lock()
	latest := r.latest
	cfg := r.state.Settings
	history := append([]historyEntry(nil), r.state.History...)
	next := r.nextProbe
	r.mu.Unlock()
	if len(latest.Credentials) == 0 {
		var err error
		latest, err = r.inspect(ctx)
		if err != nil {
			return dashboardState{}, err
		}
	}
	return dashboardState{PluginStatus: "ready", Settings: cfg, Plan: latest, History: history, NextProbeAt: next}, nil
}

func (r *runtime) inspect(ctx context.Context) (plan, error) {
	files, err := r.host.listAuth(ctx)
	if err != nil {
		return plan{}, err
	}
	r.mu.Lock()
	cfg := r.state.Settings
	quotaCache := cloneQuota(r.state.Quota)
	r.mu.Unlock()
	selectedProviders := providerSet(cfg.Providers)
	credentials := make([]credentialState, 0)
	for _, file := range files {
		if !managedAuthFile(file) {
			continue
		}
		provider := providerOf(file)
		if provider == "" || !selectedProviders[provider] {
			continue
		}
		current := tierFromPriority(file.Priority, file.Disabled)
		quota := quotaCache[file.AuthIndex]
		if quota.Status == "" {
			quota.Status = quotaUnknown
		}
		credentials = append(credentials, credentialState{
			Provider: provider, Account: maskAccount(firstText(file.Email, file.Account, file.Name)), AuthIndex: file.AuthIndex,
			CurrentTier: current, ProposedTier: current, Reason: "Waiting for quota check", Quota: quota,
			Disabled: file.Disabled, Unavailable: file.Unavailable,
		})
	}
	sortCredentials(credentials)
	return plan{GeneratedAt: time.Now().UTC(), Strategy: cfg.Strategy, Credentials: credentials}, nil
}

var errProbeInProgress = errors.New("A quota check is already in progress")

func (r *runtime) run(ctx context.Context, apply bool, trigger string) (plan, error) {
	if !r.runMu.TryLock() {
		return plan{}, errProbeInProgress
	}
	defer r.runMu.Unlock()
	files, err := r.host.listAuth(ctx)
	if err != nil {
		return plan{}, err
	}
	r.mu.Lock()
	cfg := r.state.Settings
	cache := cloneQuota(r.state.Quota)
	r.mu.Unlock()
	selectedProviders := providerSet(cfg.Providers)
	now := time.Now().UTC()
	credentials := make([]credentialState, 0)
	for _, file := range files {
		if !managedAuthFile(file) {
			continue
		}
		provider := providerOf(file)
		if provider == "" || !selectedProviders[provider] {
			continue
		}
		current := tierFromPriority(file.Priority, file.Disabled)
		quota, probeErr := probeCredential(ctx, r.host, file, cfg, now)
		if probeErr != nil {
			quota = failedQuota(cache[file.AuthIndex], probeErr, cfg.FailureThreshold, now)
		}
		cache[file.AuthIndex] = quota
		proposed, reason := chooseTier(cfg, file, current, quota, now)
		changed := proposed != current && !file.Unavailable && !(file.Disabled && proposed != tierPaused)
		credentials = append(credentials, credentialState{
			Provider: provider, Account: maskAccount(firstText(file.Email, file.Account, file.Name)), AuthIndex: file.AuthIndex,
			CurrentTier: current, ProposedTier: proposed, Reason: reason, Quota: quota,
			Disabled: file.Disabled, Unavailable: file.Unavailable, Changed: changed,
		})
	}
	sortCredentials(credentials)
	result := plan{GeneratedAt: now, Strategy: cfg.Strategy, Credentials: credentials}
	for _, credential := range credentials {
		if credential.Changed {
			result.Changes++
		}
		if credential.Quota.Status == quotaUnknown {
			result.Unknown++
		}
	}
	if apply {
		if err := r.applyPlan(ctx, files, result); err != nil {
			r.recordHistory(trigger, result.Changes, 1, "Apply failed: "+safeError(err))
			return plan{}, err
		}
		r.recordHistory(trigger, result.Changes, result.Unknown, historySummary(result))
	}
	r.mu.Lock()
	r.state.Quota = cache
	r.latest = result
	state := r.state
	r.mu.Unlock()
	if err := r.store.save(state); err != nil {
		return plan{}, err
	}
	return result, nil
}

func failedQuota(previous quotaSnapshot, probeErr error, threshold int, now time.Time) quotaSnapshot {
	failCount := previous.FailCount + 1
	message := safeError(probeErr)
	if previous.Remaining != nil && failCount < threshold {
		previous.Status = quotaCached
		previous.FailCount = failCount
		previous.LastError = message
		return previous
	}
	status := quotaRetry
	if failCount >= threshold {
		status = quotaUnknown
	}
	return quotaSnapshot{ObservedAt: now, Status: status, FailCount: failCount, LastError: message}
}

func chooseTier(cfg settings, file authFile, current tierName, quota quotaSnapshot, now time.Time) (tierName, string) {
	if file.Unavailable {
		return current, "Credential is unavailable; keeping its current tier"
	}
	if file.Disabled {
		return tierPaused, "Credential was disabled externally; it will not be reactivated automatically"
	}
	if quota.Status == quotaUnknown || quota.Status == quotaRetry || quota.Remaining == nil {
		return current, "Quota is temporarily unknown; keeping the current tier"
	}
	remaining := *quota.Remaining
	if remaining <= 0 {
		return tierPaused, "Quota exhausted; waiting for reset"
	}
	switch cfg.Strategy {
	case strategyRotate:
		return tierRegular, "Healthy credentials rotate at the same tier"
	case strategyReset:
		if quota.ResetAt != nil && quota.ResetAt.After(now) && quota.ResetAt.Sub(now) <= 24*time.Hour {
			return tierPrimary, "Quota resets within 24 hours and has remaining capacity"
		}
		return tierRegular, "Healthy credential in regular use"
	case strategyManual:
		if selected, ok := cfg.ManualTiers[file.AuthIndex]; ok {
			return selected, "Manually assigned"
		}
		return current, "No manual tier selected"
	default:
		switch {
		case remaining >= 50:
			return tierPrimary, "At least 50% quota remaining"
		case remaining >= 20:
			return tierRegular, "20%–49% quota remaining"
		default:
			return tierBackup, "1%–19% quota remaining"
		}
	}
}

func (r *runtime) applyPlan(ctx context.Context, files []authFile, result plan) error {
	byIndex := make(map[string]authFile, len(files))
	for _, file := range files {
		byIndex[file.AuthIndex] = file
	}
	for _, credential := range result.Credentials {
		if !credential.Changed {
			continue
		}
		file := byIndex[credential.AuthIndex]
		document, err := r.host.getAuth(ctx, credential.AuthIndex)
		if err != nil {
			return err
		}
		var root map[string]any
		if err := json.Unmarshal(document.JSON, &root); err != nil {
			return err
		}
		root["priority"] = credential.ProposedTier.priority()
		// Paused is implemented as a non-selectable priority, not a hard disable.
		// That lets a depleted credential automatically return after quota reset.
		if _, exists := root["disabled"]; !exists {
			root["disabled"] = false
		}
		root["credential_tier_router"] = map[string]any{
			"managed": true, "tier": credential.ProposedTier, "updated_at": time.Now().UTC().Format(time.RFC3339),
		}
		updated, err := json.Marshal(root)
		if err != nil {
			return err
		}
		if err := r.host.saveAuth(ctx, firstText(document.Name, file.Name), updated); err != nil {
			return err
		}
	}
	return nil
}

func (r *runtime) updateSettings(next settings) error {
	if err := next.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	r.state.Settings = next
	r.latest = plan{}
	state := r.state
	r.mu.Unlock()
	if err := r.store.save(state); err != nil {
		return err
	}
	r.restartWorker()
	return nil
}

func (r *runtime) recordHistory(trigger string, changes, errorsCount int, summary string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := historyEntry{At: time.Now().UTC(), Trigger: trigger, Summary: summary, Changes: changes, Errors: errorsCount}
	r.state.History = append([]historyEntry{entry}, r.state.History...)
	if len(r.state.History) > 12 {
		r.state.History = r.state.History[:12]
	}
}

func historySummary(result plan) string {
	if result.Changes == 0 {
		return "Quota check complete; no credentials need changes"
	}
	return fmt.Sprintf("Updated %d credentials; new priorities mainly affect future requests", result.Changes)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	message = strings.ReplaceAll(message, "Bearer ", "Bearer [REDACTED]")
	if len(message) > 180 {
		message = message[:180]
	}
	return message
}

func cloneQuota(source map[string]quotaSnapshot) map[string]quotaSnapshot {
	out := make(map[string]quotaSnapshot, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func providerSet(providers []string) map[string]bool {
	out := map[string]bool{}
	for _, provider := range providers {
		out[provider] = true
	}
	return out
}

func maskAccount(account string) string {
	account = strings.TrimSpace(account)
	if at := strings.Index(account, "@"); at > 1 {
		name := account[:at]
		domain := account[at:]
		if len(name) > 2 {
			name = name[:2] + strings.Repeat("•", min(5, len(name)-2))
		}
		return name + domain
	}
	if len(account) <= 5 {
		return account
	}
	return account[:3] + "•••" + account[len(account)-2:]
}

func sortCredentials(credentials []credentialState) {
	sort.SliceStable(credentials, func(i, j int) bool {
		if credentials[i].Provider != credentials[j].Provider {
			return credentials[i].Provider < credentials[j].Provider
		}
		return credentials[i].Account < credentials[j].Account
	})
}

func success(result any) []byte {
	raw, err := json.Marshal(result)
	if err != nil {
		return failure("internal_error", err.Error(), false)
	}
	envelope, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(raw)})
	return envelope
}

func failure(code, message string, retryable bool) []byte {
	raw, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message, "retryable": retryable}})
	return raw
}
