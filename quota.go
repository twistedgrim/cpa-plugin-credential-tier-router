package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var antigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

type authMaterial struct {
	AccessToken string
	AccountID   string
	ProjectID   string
}

func parseAuthMaterial(raw json.RawMessage) authMaterial {
	var document struct {
		AccessToken    string `json:"access_token"`
		AccountID      string `json:"account_id"`
		ProjectID      string `json:"project_id"`
		QuotaProjectID string `json:"quota_project_id"`
		Project        string `json:"project"`
	}
	_ = json.Unmarshal(raw, &document)
	project := firstText(document.ProjectID, document.QuotaProjectID, document.Project)
	return authMaterial{AccessToken: strings.TrimSpace(document.AccessToken), AccountID: strings.TrimSpace(document.AccountID), ProjectID: project}
}

func firstText(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func probeCredential(ctx context.Context, host hostAPI, file authFile, cfg settings, now time.Time) (quotaSnapshot, error) {
	document, err := host.getAuth(ctx, file.AuthIndex)
	if err != nil {
		return quotaSnapshot{}, err
	}
	material := parseAuthMaterial(document.JSON)
	switch providerOf(file) {
	case "codex":
		return probeCodex(ctx, host, file.AuthIndex, material, now)
	case "antigravity":
		return probeAntigravity(ctx, host, file.AuthIndex, material, cfg.AntigravityGroup, now)
	default:
		return quotaSnapshot{}, errors.New("unsupported provider")
	}
}

func probeCodex(ctx context.Context, host hostAPI, authIndex string, material authMaterial, now time.Time) (quotaSnapshot, error) {
	token := material.AccessToken
	if token == "" {
		token = "$TOKEN$"
	}
	headers := hostHeader{
		"Accept":        {"application/json"},
		"Authorization": {"Bearer " + token},
		"Content-Type":  {"application/json"},
		"User-Agent":    {"codex_cli_rs/0.76.0 (Linux; x86_64)"},
	}
	if material.AccountID != "" {
		headers["Chatgpt-Account-Id"] = []string{material.AccountID}
	}
	response, err := host.httpDo(ctx, hostHTTPRequest{AuthIndex: authIndex, Method: http.MethodGet, URL: "https://chatgpt.com/backend-api/wham/usage", Headers: headers})
	if err != nil {
		return quotaSnapshot{}, errors.New("Codex quota request failed")
	}
	if response.StatusCode != http.StatusOK {
		return quotaSnapshot{}, fmt.Errorf("Codex quota API returned %d", response.StatusCode)
	}
	remaining, resetAt, ok := parseCodexQuota(response.Body, now)
	if !ok {
		return quotaSnapshot{}, errors.New("Codex quota response has no usable window")
	}
	return readyQuota(remaining, resetAt, now), nil
}

func probeAntigravity(ctx context.Context, host hostAPI, authIndex string, material authMaterial, group string, now time.Time) (quotaSnapshot, error) {
	token := material.AccessToken
	if token == "" {
		token = "$TOKEN$"
	}
	headers := hostHeader{
		"Accept":        {"application/json"},
		"Authorization": {"Bearer " + token},
		"Content-Type":  {"application/json"},
		"User-Agent":    {"antigravity/cli/1.0.8 linux/amd64"},
	}
	var body []byte
	if material.ProjectID != "" {
		body, _ = json.Marshal(map[string]string{"project": material.ProjectID})
	}
	lastStatus := 0
	for _, endpoint := range antigravityQuotaURLs {
		response, err := host.httpDo(ctx, hostHTTPRequest{AuthIndex: authIndex, Method: http.MethodPost, URL: endpoint, Headers: headers, Body: body})
		if err != nil {
			return quotaSnapshot{}, errors.New("Antigravity quota request failed")
		}
		lastStatus = response.StatusCode
		if response.StatusCode != http.StatusOK {
			continue
		}
		remaining, resetAt, ok := parseAntigravityQuota(response.Body, group)
		if ok {
			return readyQuota(remaining, resetAt, now), nil
		}
	}
	return quotaSnapshot{}, fmt.Errorf("Antigravity quota API returned %d", lastStatus)
}

func readyQuota(remaining int, resetAt *time.Time, now time.Time) quotaSnapshot {
	remaining = max(0, min(100, remaining))
	return quotaSnapshot{Remaining: &remaining, ResetAt: resetAt, ObservedAt: now.UTC(), Status: quotaReady}
}

func parseCodexQuota(raw []byte, now time.Time) (int, *time.Time, bool) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return 0, nil, false
	}
	rateLimit, _ := root["rate_limit"].(map[string]any)
	type candidate struct {
		remaining int
		resetAt   *time.Time
	}
	candidates := []candidate{}
	for _, key := range []string{"primary_window", "secondary_window"} {
		window, _ := rateLimit[key].(map[string]any)
		if window == nil {
			continue
		}
		remaining, ok := quotaPercent(window)
		resetAt := quotaResetAt(window, now)
		if ok && resetAt != nil {
			candidates = append(candidates, candidate{remaining: remaining, resetAt: resetAt})
		}
	}
	if len(candidates) == 0 {
		return 0, nil, false
	}
	selected := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.remaining < selected.remaining {
			selected = candidate
		}
	}
	return selected.remaining, selected.resetAt, true
}

func quotaPercent(window map[string]any) (int, bool) {
	if reached, ok := toBool(window["limit_reached"]); ok && reached {
		return 0, true
	}
	if used, ok := toFloat(window["used_percent"]); ok {
		return int(math.Ceil(max(0, 100-used))), true
	}
	remaining, hasRemaining := toFloat(window["remaining"])
	limit, hasLimit := toFloat(window["limit"])
	if hasRemaining && hasLimit && limit > 0 {
		return int(math.Ceil(max(0, min(100, remaining/limit*100)))), true
	}
	if hasRemaining && remaining >= 0 && remaining <= 100 {
		return int(math.Ceil(remaining)), true
	}
	used, hasUsed := toFloat(window["used"])
	if hasUsed && hasLimit && limit > 0 {
		return int(math.Ceil(max(0, min(100, (limit-used)/limit*100)))), true
	}
	return 0, false
}

func quotaResetAt(window map[string]any, now time.Time) *time.Time {
	if value, ok := parseTime(window["reset_at"]); ok {
		return value
	}
	if seconds, ok := toFloat(window["reset_after_seconds"]); ok && seconds > 0 {
		value := now.UTC().Add(time.Duration(seconds) * time.Second)
		return &value
	}
	return nil
}

func parseAntigravityQuota(raw []byte, group string) (int, *time.Time, bool) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return 0, nil, false
	}
	type candidate struct {
		remaining int
		resetAt   *time.Time
	}
	candidates := []candidate{}
	add := func(modelText string, remainingRaw, resetRaw any) {
		if !matchesModelGroup(modelText, group) {
			return
		}
		remainingValue, ok := toFloat(remainingRaw)
		if !ok {
			return
		}
		if remainingValue <= 1 {
			remainingValue *= 100
		}
		resetAt, ok := parseTime(resetRaw)
		if !ok {
			return
		}
		candidates = append(candidates, candidate{remaining: int(math.Ceil(max(0, min(100, remainingValue)))), resetAt: resetAt})
	}
	if models, ok := root["models"].(map[string]any); ok {
		for modelID, rawModel := range models {
			model, _ := rawModel.(map[string]any)
			quota, _ := model["quotaInfo"].(map[string]any)
			text := modelID + " " + fmt.Sprint(model["modelProvider"])
			if windows, ok := quota["windows"].([]any); ok && len(windows) > 0 {
				for _, rawWindow := range windows {
					window, _ := rawWindow.(map[string]any)
					add(text, window["remainingFraction"], window["resetTime"])
				}
			} else {
				add(text, quota["remainingFraction"], quota["resetTime"])
			}
		}
	}
	if buckets, ok := root["buckets"].([]any); ok {
		for _, rawBucket := range buckets {
			bucket, _ := rawBucket.(map[string]any)
			add(fmt.Sprint(bucket["modelId"]), bucket["remainingFraction"], bucket["resetTime"])
		}
	}
	if groups, ok := root["groups"].([]any); ok {
		for _, rawGroup := range groups {
			quotaGroup, _ := rawGroup.(map[string]any)
			text := fmt.Sprint(quotaGroup["displayName"]) + " " + fmt.Sprint(quotaGroup["description"])
			buckets, _ := quotaGroup["buckets"].([]any)
			for _, rawBucket := range buckets {
				bucket, _ := rawBucket.(map[string]any)
				add(text, bucket["remainingFraction"], bucket["resetTime"])
			}
		}
	}
	if len(candidates) == 0 {
		return 0, nil, false
	}
	selected := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.remaining < selected.remaining {
			selected = candidate
		}
	}
	return selected.remaining, selected.resetAt, true
}

func matchesModelGroup(text, group string) bool {
	text = strings.ToLower(text)
	if group == "claude_gpt" {
		return strings.Contains(text, "claude") || strings.Contains(text, "gpt") || strings.Contains(text, "openai")
	}
	return strings.Contains(text, "gemini") && !strings.Contains(text, "claude") && !strings.Contains(text, "gpt")
}

func parseTime(raw any) (*time.Time, bool) {
	switch value := raw.(type) {
	case json.Number:
		integer, err := value.Int64()
		if err != nil {
			return nil, false
		}
		return unixTime(integer)
	case float64:
		return unixTime(int64(value))
	case string:
		value = strings.TrimSpace(value)
		if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
			return unixTime(integer)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, value); err == nil {
				parsed = parsed.UTC()
				return &parsed, true
			}
		}
	}
	return nil, false
}

func unixTime(value int64) (*time.Time, bool) {
	if value <= 0 {
		return nil, false
	}
	var parsed time.Time
	if value > 1_000_000_000_000 {
		parsed = time.UnixMilli(value).UTC()
	} else {
		parsed = time.Unix(value, 0).UTC()
	}
	return &parsed, true
}

func toFloat(raw any) (float64, bool) {
	switch value := raw.(type) {
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	case float64:
		return value, true
	case int:
		return float64(value), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func toBool(raw any) (bool, bool) {
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		return parsed, err == nil
	default:
		return false, false
	}
}
