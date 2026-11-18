package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column: the
// filtering conditions for a channel instance. All fields are optional, and the default
// means "no filtering" — this is exactly the fallback semantics for malformed config,
// see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty means
	// no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs: empty arrays mean unrestricted; non-empty requires an
	// intersection with the event.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude: empty means accept all; non-empty requires vulnclass to match
	// any of the keywords. VulnClassExclude excludes on a match of any keyword
	// (exclusion takes priority over inclusion). Matching is a case-insensitive
	// substring — safer than regex: a badly configured regex cannot silently disable the
	// channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange determines whether the channel receives finding status-change
	// events (meaningful only in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses a channel's filtering configuration.
//
// **It never returns an error.** This is a deliberate design choice: malformed
// filtering config always degrades to the zero-value Filter (= no filtering = match
// everything), because for a finding notification system **one extra delivery is far
// better than silently missing a critical one**. Turning a parse failure into "don't
// deliver" would give users a channel that looks configured but delivers nothing — the
// worst possible failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On a parse failure f stays at its zero value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (an empty string
// means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the **value-constrained** fields of the filter config; called when
// saving a channel.
//
// Why it must be blocked at write time: Match treats an unknown threshold as
// `rank >= 0`, which is always true — so a typo in min_severity (e.g. "hgih") makes
// the filter **silently fail open** into "deliver everything". This is consistent with
// this package's "better to over-deliver than to miss" tradeoff (nothing is missed),
// but the consequence is that users think they are doing tiered delivery while in fact
// flooding the group with every finding, with no sign that they misconfigured it. This
// kind of "silent degradation" should be caught at the entry point.
//
// Note that Validate is only used on the **write** path. The read path still follows
// ParseFilter's lenient semantics, so bad values already present in historical data do
// not make the whole channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("invalid minimum severity %q; available: low / medium / high / critical, or leave blank for no threshold", f.MinSeverity)
	}
	return nil
}

// Match decides whether an event should be delivered to a channel with this filter.
//
// **It never returns an error**, for the same reason as ParseFilter: any internal
// anomaly is treated as a "match". Decision order: event type → severity threshold →
// task/asset scope → vulnerability-class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that explicitly opt in.
	// Off by default, because most users understand "deliver" to mean "a new finding
	// was discovered", not a running log of every status transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion takes priority: matching any exclusion keyword disqualifies the event,
	// even if it also matches the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan is fine for these small sets; both sides are on the scale of
	// "a few dozen manually ticked items", where building a map costs more than it saves.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of the keywords (case-insensitively).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
