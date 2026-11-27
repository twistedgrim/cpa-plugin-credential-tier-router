package main

import "time"

type tierName string

const (
	tierPrimary tierName = "primary"
	tierRegular tierName = "regular"
	tierBackup  tierName = "backup"
	tierPaused  tierName = "paused"
)

func (t tierName) valid() bool {
	switch t {
	case tierPrimary, tierRegular, tierBackup, tierPaused:
		return true
	default:
		return false
	}
}

func (t tierName) priority() int {
	switch t {
	case tierPrimary:
		return 400
	case tierRegular:
		return 300
	case tierBackup:
		return 200
	case tierPaused:
		return -1
	default:
		return 300
	}
}

func tierFromPriority(priority int, disabled bool) tierName {
	if disabled || priority < 0 {
		return tierPaused
	}
	switch {
	case priority >= 400:
		return tierPrimary
	case priority >= 300:
		return tierRegular
	case priority >= 200:
		return tierBackup
	default:
		return tierRegular
	}
}

type quotaStatus string

const (
	quotaReady   quotaStatus = "ready"
	quotaCached  quotaStatus = "cached"
	quotaRetry   quotaStatus = "retrying"
	quotaUnknown quotaStatus = "unknown"
)

type quotaSnapshot struct {
	Remaining  *int        `json:"remaining,omitempty"`
	ResetAt    *time.Time  `json:"reset_at,omitempty"`
	ObservedAt time.Time   `json:"observed_at"`
	Status     quotaStatus `json:"status"`
	FailCount  int         `json:"fail_count,omitempty"`
	LastError  string      `json:"last_error,omitempty"`

	CodexWindows      map[string]*quotaWindow `json:"codex_windows,omitempty"`
	CodexLimitReached bool                    `json:"codex_limit_reached,omitempty"`
}

type quotaWindow struct {
	Remaining int       `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

type credentialState struct {
	Provider     string        `json:"provider"`
	Account      string        `json:"account"`
	AuthIndex    string        `json:"auth_index"`
	CurrentTier  tierName      `json:"current_tier"`
	ProposedTier tierName      `json:"proposed_tier"`
	Reason       string        `json:"reason"`
	Quota        quotaSnapshot `json:"quota"`
	Disabled     bool          `json:"disabled"`
	Unavailable  bool          `json:"unavailable"`
	Changed      bool          `json:"changed"`
}

type plan struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Strategy    strategyName      `json:"strategy"`
	Credentials []credentialState `json:"credentials"`
	Changes     int               `json:"changes"`
	Unknown     int               `json:"unknown"`
}

type historyEntry struct {
	At      time.Time `json:"at"`
	Trigger string    `json:"trigger"`
	Summary string    `json:"summary"`
	Changes int       `json:"changes"`
	Errors  int       `json:"errors"`
}

type persistedState struct {
	Settings settings                 `json:"settings"`
	Quota    map[string]quotaSnapshot `json:"quota"`
	History  []historyEntry           `json:"history"`
}

type dashboardState struct {
	PluginStatus string         `json:"plugin_status"`
	Settings     settings       `json:"settings"`
	Plan         plan           `json:"plan"`
	History      []historyEntry `json:"history"`
	NextProbeAt  *time.Time     `json:"next_probe_at,omitempty"`
}
