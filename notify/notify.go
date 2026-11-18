// Package notify implements the IM / email delivery channel adapters for finding notifications.
//
// Layering: this package is a **leaf package** that depends only on the standard library.
// It knows nothing about the database or the server. Channel configuration is passed in
// as a map[string]any (corresponding to the notification_channels.config JSONB column),
// and the content to deliver is passed in as a Message. The benefit of this split is that
// the truly error-prone parts — signature calculation, UTF-8 truncation, filter matching —
// can be unit-tested without PostgreSQL, with the host only doing orchestration on the
// server side.
//
// Concurrency contract: Channel implementations must be **stateless**. The same Channel
// instance is reused concurrently by multiple channel configurations (even by multiple bot
// instances of the same channel), so all credentials must be passed in via cfg; caching a
// webhook URL or similar in the implementation's own fields is not allowed.
package notify

// Channel type identifiers. The values are also the valid set for
// notification_channels.kind, validated by a whitelist on the server side (same as
// findings.status: no DB CHECK, so new channels can be added later).
const (
	KindDingTalk = "dingtalk" // DingTalk custom bot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom bot
	KindWeCom    = "wecom"    // WeCom group bot
	KindWebhook  = "webhook"  // Generic Webhook: custom method/headers/JSON template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP email
)

// Event types, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the fallback value for an empty kind in config.
const InitKind = KindDingTalk

// severityRank maps finding severity to a comparable ordinal. Unknown severities
// return 0, so any min_severity setting filters them out — when in doubt, don't deliver,
// to avoid flooding with false positives.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank returns the ordinal for a severity; unknown severities return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns an emoji-prefixed English severity label, used in message
// titles and card colors. Unknown severities are echoed as-is, never invented.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 Critical"
	case "high":
		return "🟠 High"
	case "medium":
		return "🟡 Medium"
	case "low":
		return "🔵 Low"
	default:
		return severity
	}
}

// StatusLabel translates a handling status into English, used in status-change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "Pending"
	case "in_progress":
		return "In progress"
	case "confirmed":
		return "Confirmed"
	case "resolved":
		return "Resolved"
	case "fixed":
		return "Fixed"
	case "false_positive":
		return "False positive"
	case "ignored":
		return "Ignored"
	case "duplicate":
		return "Duplicate"
	case "risk_accepted":
		return "Risk accepted"
	default:
		return status
	}
}

// AtLeast reports whether severity meets the min threshold. An empty min means no
// threshold, so everything passes. Note that an unknown severity has ordinal 0 and
// will be rejected by any non-empty min (see the severityRank comment).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
