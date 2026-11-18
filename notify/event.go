package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. It is
// written by the db layer's finding-persistence transaction and read by the server
// layer's delivery engine and filter matching. The definition lives in this package
// because it is the payload of the "notification domain": db only serializes it and
// does not interpret the fields.
//
// Why finding fields are stored redundantly instead of being looked up at render time:
// findings can later be renamed, have their severity changed, or have their status
// changed, while the notification should reflect the conclusion **at the time of the
// event** — a lookup would give the dangerous impression of "later changed to low".
// In addition, fan-out and rendering no longer need to JOIN the findings/tasks/assets
// tables.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is one finding to be delivered, for channel rendering.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets holds resolved display names for the assets (e.g. domain/IP). It is
	// filled in by the server layer — this package does not touch the database and
	// cannot obtain the names.
	Assets []string
	// DetailURL is the deep link to the finding details; empty means public_base_url is
	// not configured, so it is omitted when rendering.
	DetailURL string
	// Only for status-change events; when both are non-empty, rendered as "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this item is a status-change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the item's display title: the human-assigned name first, falling back
// to the vulnerability class (vulnclass), and a placeholder when both are empty — an
// empty title is never emitted.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(Untitled finding)"
}

// Message is the complete content of one channel send.
type Message struct {
	// Length 1 for a single delivery; a whole batch for a digest delivery.
	// An empty slice is invalid; the caller must ensure at least one item.
	Items []Item
	// When Batch=true, rendered as a digest message (different title, time window and
	// item count).
	Batch bool
	// WindowMinutes is the digest interval in minutes; only used for the "last N
	// minutes" text when Batch=true. It is deliberately passed in explicitly from config
	// rather than computed with time.Since at render time, so rendering stays deterministic
	// and testable.
	WindowMinutes int
	// HomeURL is the platform dashboard URL (the global public_base_url); empty means no
	// dashboard link is included.
	HomeURL string
}
