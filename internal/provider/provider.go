// Package provider implements ferridex's upstream subscription channels
// (Codex, Claude, Cursor) and the shared contract the server routes against.
package provider

import (
	"context"
	"net/http"
)

// Provider is one upstream subscription channel (Codex, Claude or Cursor).
type Provider interface {
	Name() string
	Relay(w http.ResponseWriter, r *http.Request)
	Status() ProviderStatus
}

// UsageQuerier is implemented by providers whose subscription usage can be
// fetched on demand from the dashboard's 查询用量 button (Codex, Claude, agy).
// Usage is never polled automatically — only an explicit query hits upstream.
type UsageQuerier interface {
	QueryUsage(ctx context.Context) ([]UsageWindow, error)
}

// UsageDetailQuerier is an optional extension of UsageQuerier returning richer
// subscription metadata (plan tier, account email, credits, per-pool models).
type UsageDetailQuerier interface {
	UsageQuerier
	QueryUsageReport(ctx context.Context) (UsageReport, error)
}

// UsageExtra is one supplemental key/value badge shown below quota windows.
type UsageExtra struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Tone  string `json:"tone,omitempty"` // "ok" | "warn" | "bad"
}

// UsageReport holds detailed subscription quota data for one provider.
type UsageReport struct {
	Plan    string        `json:"plan,omitempty"`
	Account string        `json:"account,omitempty"`
	Status  string        `json:"status,omitempty"`
	Extras  []UsageExtra  `json:"extras,omitempty"`
	Windows []UsageWindow `json:"windows"`
}

// ProviderStatus is what the dashboard shows for one provider. Usage windows
// are deliberately absent: they are only returned by an explicit QueryUsage.
type ProviderStatus struct {
	Name          string   `json:"name"`
	Title         string   `json:"title"`
	LoggedIn      bool     `json:"logged_in"`
	Account       string   `json:"account,omitempty"`
	Detail        string   `json:"detail,omitempty"`
	Models        []string `json:"models,omitempty"`
	Endpoint      string   `json:"endpoint"`
	SupportsUsage bool     `json:"supports_usage,omitempty"`
}

// UsageWindow is one subscription rate-limit window shown as a progress bar on
// the dashboard (e.g. Claude's 5-hour / 7-day windows, Codex's primary/weekly,
// Antigravity's model quota pools).
type UsageWindow struct {
	Label         string   `json:"label"`                    // "5 小时" / "7 天" / "每周" / "Gemini 配额"
	Utilization   float64  `json:"utilization"`              // 0..1 fraction used
	ResetsAt      int64    `json:"resets_at"`                // unix seconds, 0 if unknown
	WindowSeconds int64    `json:"window_seconds,omitempty"` // total window length in seconds, 0 if unknown
	Detail        string   `json:"detail,omitempty"`         // supplemental per-window note
	Models        []string `json:"models,omitempty"`         // models sharing this quota pool
	Group         string   `json:"group,omitempty"`          // optional grouping tag
}
