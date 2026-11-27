package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	pluginID = "credential-tier-router"
)

var pluginVersion = "0.1.3"

type strategyName string

const (
	strategyQuota  strategyName = "quota_bands"
	strategyRotate strategyName = "balanced"
	strategyReset  strategyName = "reset_soon"
	strategyManual strategyName = "manual"
)

type settings struct {
	AutoApply        bool                `json:"auto_apply"`
	Strategy         strategyName        `json:"strategy"`
	IntervalMinutes  int                 `json:"interval_minutes"`
	Providers        []string            `json:"providers"`
	AntigravityGroup string              `json:"antigravity_group"`
	FailureThreshold int                 `json:"failure_threshold"`
	ManualTiers      map[string]tierName `json:"manual_tiers,omitempty"`
}

func defaultSettings() settings {
	return settings{
		AutoApply:        false,
		Strategy:         strategyQuota,
		IntervalMinutes:  15,
		Providers:        []string{"codex", "antigravity"},
		AntigravityGroup: "gemini",
		FailureThreshold: 3,
		ManualTiers:      map[string]tierName{},
	}
}

func (s settings) validate() error {
	switch s.Strategy {
	case strategyQuota, strategyRotate, strategyReset, strategyManual:
	default:
		return fmt.Errorf("unknown strategy %q", s.Strategy)
	}
	if s.IntervalMinutes < 5 || s.IntervalMinutes > 1440 {
		return errors.New("interval_minutes must be between 5 and 1440")
	}
	if s.FailureThreshold < 2 || s.FailureThreshold > 10 {
		return errors.New("failure_threshold must be between 2 and 10")
	}
	if s.AntigravityGroup != "gemini" && s.AntigravityGroup != "claude_gpt" {
		return errors.New("antigravity_group must be gemini or claude_gpt")
	}
	if len(s.Providers) == 0 {
		return errors.New("at least one provider must be selected")
	}
	seen := map[string]bool{}
	for _, provider := range s.Providers {
		if provider != "codex" && provider != "antigravity" {
			return fmt.Errorf("unsupported provider %q", provider)
		}
		if seen[provider] {
			return fmt.Errorf("duplicate provider %q", provider)
		}
		seen[provider] = true
	}
	for authIndex, tier := range s.ManualTiers {
		if strings.TrimSpace(authIndex) == "" || !tier.valid() {
			return errors.New("manual_tiers contains an invalid entry")
		}
	}
	return nil
}

func parsePluginConfig(raw []byte) (settings, error) {
	cfg := defaultSettings()
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return cfg, nil
	}
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &cfg); err != nil {
			return settings{}, err
		}
		if cfg.ManualTiers == nil {
			cfg.ManualTiers = map[string]tierName{}
		}
		return cfg, cfg.validate()
	}
	values := map[string]string{}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" || strings.HasSuffix(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(parts[0]), "\"'")
		value := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		values[key] = value
	}
	if value := values["auto_apply"]; value != "" {
		cfg.AutoApply = parseBool(value)
	}
	if value := values["strategy"]; value != "" {
		cfg.Strategy = strategyName(value)
	}
	if value := values["interval_minutes"]; value != "" {
		cfg.IntervalMinutes, _ = strconv.Atoi(value)
	} else if value := values["interval"]; value != "" {
		value = strings.TrimSuffix(value, "m")
		cfg.IntervalMinutes, _ = strconv.Atoi(value)
	}
	if value := values["provider_scope"]; value != "" {
		cfg.Providers = splitProviders(value)
	}
	if value := values["antigravity_group"]; value != "" {
		cfg.AntigravityGroup = value
	}
	if value := values["failure_threshold"]; value != "" {
		cfg.FailureThreshold, _ = strconv.Atoi(value)
	}
	return cfg, cfg.validate()
}

func splitProviders(value string) []string {
	value = strings.ReplaceAll(value, ",", "|")
	parts := strings.Split(value, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if provider := strings.ToLower(strings.TrimSpace(part)); provider != "" {
			out = append(out, provider)
		}
	}
	return out
}

func parseBool(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "true" || value == "1" || value == "yes" || value == "on"
}

func (s settings) interval() time.Duration {
	return time.Duration(s.IntervalMinutes) * time.Minute
}
