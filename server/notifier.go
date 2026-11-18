package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global setting keys (stored in the settings key-value table; no dedicated table needed).
const (
	// settingNotifyEnabled is the master switch for notifications. On by default: it is
	// a one-click kill switch for maintenance, not the condition that enables the feature
	// — the real condition is "whether any channel is configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the externally accessible address used to build deep
	// links to finding details (e.g. https://artex.example.com). If blank, messages carry
	// no deep-link button. The project has no reusable external address setting, so a new
	// one is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest interval in minutes.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is the upper bound
	// on this engine's realtime responsiveness and the main source of latency between
	// "finding persisted" and "message arrives in IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming deliveries. It must be significantly
	// longer than the worst-case single delivery time (the notify package's HTTP client
	// timeout is 15 seconds), otherwise the same row could be delivered by two dispatchers
	// at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits the number of events fanned out per tick, so enabling a
	// channel for the first time does not expand the entire historical backlog into
	// delivery tasks at once.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default digest interval.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-tick delivery cap for channels with no rate
	// limit. It exists to prevent "one channel set to unlimited + a scan producing
	// thousands of findings" from turning a single tick into a long blockage.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the maximum number of deliveries per channel per
	// tick.
	//
	// This cap is derived from the **lease duration**: claiming a row applies a lease
	// (notifyLease = 3 minutes); if a tick serially delivers so many items that the
	// worst-case time exceeds the lease, the lease expires before the last ones are sent.
	// In a single process this does not matter (Run is a single goroutine and ticks do not
	// re-enter), but with **two processes connected to the same database** the peer will
	// reclaim expired rows and send them again, double-increment attempts, and mark them
	// failed while the original process is still delivering.
	//
	// Value: 3-minute lease / 30-second per-send timeout = 6 would **use up the entire
	// lease** with zero margin, so it cannot be used; 5 leaves a 30-second margin on the
	// 150-second worst case. This relationship is pinned by
	// TestNotifyTickBudgetFitsWithinLease — changing notifyLease, notifySendTimeout or this
	// value will make that assertion fail.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value
	// of the previous constant; their product must not exceed notifyLease, see
	// TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by attempts made.
// Three attempts (including the first) correspond to db.MaxNotifyAttempts; the two must
// be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for finding notifications.
//
// It runs alongside Scheduler as a separate goroutine (see server.New). Scheduler's tick
// is deliberately not reused: the realtime requirement for notifications (3 seconds)
// differs from the trigger's business cadence, and their failures should not be coupled
// — a stuck notification should not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. The number of channels is small and contention low, so one
	// mutex is enough; a finer-grained structure is not worth it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket for a single channel.
//
// A token bucket is used instead of a "count per minute then reset" sliding window,
// because the latter has bad boundary effects: sending 20 messages at the end of a window
// and another 20 the next instant looks like 40 in one second to the platform and gets
// rate-limited; a token bucket refills at a constant rate and naturally avoids bursts.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: first fan out new events, then deliver due tasks.
//
// A failure in any step is only logged, never interrupts the loop — a notification system
// failure must never escalate into a process-level problem. Each tick is independent and
// the next round retries naturally.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to fan out events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket measures **number of messages** (equivalent to HTTP requests),
		// not number of findings. In realtime mode they are the same (one finding, one
		// message); in digest mode a whole batch of findings becomes one message and consumes
		// only one token.
		//
		// Both modes ask the token bucket first, then claim by the allowed amount — the
		// order cannot be reversed, otherwise deliveries blocked by rate limiting would
		// already have consumed a retry attempt.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns this tick's token consumption and batch-size upper bound for a
// digest channel.
//
// The two return values are **two different dimensions**, which is why this is a separate
// function:
//
//   - tokens is a number of messages. A batch of findings becomes one message and one
//     HTTP request, so it is always 1. rate_per_min therefore still applies to digest (at
//     most that many digest messages per minute).
//   - claimLimit is the maximum number of findings in one batch. It is bounded only by
//     memory, not by the request budget.
//
// To make rate_per_min take effect for digest, the per-tick request budget
// (notifyMaxSendsPerChannelPerTick, derived from the lease) was once passed directly as
// the batch size. The consequence: a channel with rate_per_min=20 refills only 1 token in
// a 3-second tick, so each digest message held just 1 finding — digest degraded into
// "realtime delivery with digest-style text", readers got a string of "1 new finding in
// the last 30 minutes", and db.MaxDigestBatchSize was never reachable.
//
// This symptom is hard to catch in end-to-end tests (existing cases all pass a large
// limit to stepDigest manually, bypassing the budget calculation in step), so the decision
// is centralized here and pinned directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers realtime tasks for a channel, one finding per message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// A render failure is a local data problem; retrying will not help.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates a channel's pending deliveries into one message and sends it when
// the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to check digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and did not make it into the message must be
	// explicitly marked failed. Otherwise they stay outside included — in neither the
	// message nor the failure list — and on a successful send their state is missed by the
	// later bulk marking, leaving them stuck in sending until the lease expires and they
	// are claimed again and again.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot could not be parsed; this finding could not be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad-snapshot deliveries channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only pass the items that made it into the message to send: included[i] corresponds
	// strictly to msg.Items[i], and send relies on this mapping to apply "the channel
	// reported it fitted the first K items" to the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions state based on the result.
//
// All deliveries in one batch (possibly dozens in digest mode) share one send result:
// either delivered, or the whole batch is retried. No per-item retry is done — a digest
// message is one message, and resending only part of it would break batch semantics.
//
// The sole exception is **segmentation caused by the channel's length limit**: if the
// channel reports it fitted only the first K items, then item K+1 onward must be left for
// the next batch rather than marked as sent along with the rest. Otherwise the truncated
// findings are neither in the message nor in the failure list — completely gone.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// A per-delivery timeout prevents one stuck channel from holding up all remaining
	// channels this tick.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// A channel cannot report more delivered items than there are deliveries; if it
			// does, the renderer miscounted. Treat all as delivered and log it — better than
			// corrupting the records.
			log.Printf("[notify] channel reported %d delivered items, more than the %d deliveries for channel=%s; treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark deliveries as sent channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message reached the channel's length limit: requeue the rest immediately
			// for the next tick. Use DeferDeliveries rather than RescheduleDeliveries — this
			// is not a failure and must not consume the retry budget (claiming already
			// incremented attempts optimistically; that is undone here).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message reached the channel length limit; only the first %d items were delivered, the rest are deferred to the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to queue deferred deliveries channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither reported an error nor a delivered count. Treat as a failure
		// (with backoff) so this delivery is not claimed repeatedly yet never marked off.
		err = fmt.Errorf("channel did not report the number of delivered items (delivered=%d)", delivered)
	}

	// Failure handling is decided **per item**, not by the batch's maximum attempt count.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, condemning the whole
	// batch, but items in a batch have different attempt counts: an old delivery already
	// retried twice (attempts=2) would drag fresh deliveries (attempts=1) in the same batch
	// into failed — a new finding lost permanently without using a single retry, the exact
	// opposite of the intent to "not let old rows sink new ones".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] failed to mark deliveries as failed channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failing after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] failed to mark deliveries as failed channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Reschedule grouped by delay: with only 3 backoff tiers the number of groups is
	// naturally small, so there is no need to issue one UPDATE per item (which would mean
	// 500 round trips for a 500-item batch).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to reschedule deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent=%d retries_exhausted=%d pending_retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns the elements of all that are not in keep (compared by pointer
// identity). It is used to find deliveries that "did not make it into the message" — they
// must be explicitly handled, not left in a gray area.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt looks up the channel implementation and parses its config. ok=false means the
// type is not registered and the delivery should be failed outright rather than retried
// forever.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On a config parse failure, use an empty map: the channel's own Validate will report
		// "which field is missing", which guides the user better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single-finding message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a digest message. Snapshots are parsed one by one — a bad one only
// skips that item, without dragging down the whole digest.
//
// The returned included and msg.Items **correspond strictly one-to-one** (the i-th
// delivery ↔ the i-th item). This mapping is a hard requirement: the caller decides which
// first K deliveries to mark as sent from "the channel reported it fitted the first K
// items". If a bad snapshot were skipped here without also removing its delivery from
// included, the indices would shift — the bad item that should fail would be marked sent,
// and a good item would be wrongly treated as unsent. The broken ones are explicitly
// marked failed by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// A bad snapshot goes into neither the message nor included — its handling is the
			// caller's responsibility (mark it failed explicitly rather than letting it slip
			// through as "delivered").
			log.Printf("[notify] skipping unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in the digest batch could not be parsed", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a deliverable item, also resolving asset names
// and the deep link.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Failure to resolve asset names should not block delivery: missing a name is much
		// less serious than missing the notification — the message just has one less asset
		// line.
		log.Printf("[notify] failed to resolve asset names finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The detail page route is in web/src/app/(main)/function/findings/detail/page.tsx;
		// it reads the finding id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want tokens** from a channel's bucket and returns the number
// actually taken.
//
// One token = one message (one HTTP request). In realtime mode the caller passes however
// many it needs; in digest mode a whole batch of findings sends just one message, so it
// passes 1.
//
// The bucket capacity is the channel's per-minute limit, refilling at a constant rate.
// ratePerMin<=0 means no limit; a finite but large-enough value is returned so an
// unbounded backlog cannot stall a single tick.
//
// The want cap is necessary: without it the bucket would have to be drained completely,
// while the caller also has a per-tick cap — extra tokens would go unused and vanish before
// the next refill, so the accumulated burst capacity would never be reachable, and even a
// tick with nothing pending would still be charged.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill by the actual elapsed time, at ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: token counts accumulate in floating point, so
	// refilling in two halves can give 0.5 + 0.5 = 0.9999999999, which int() truncates to 0
	// — a mathematically full bucket would yield no token. 1e-9 is far smaller than one
	// token and will not mask a real deficit.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for deep links, with trailing slashes
// removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default when invalid or
// unset.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the event snapshot for a delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event row is authoritative for the event type; the snapshot's copy may have
		// been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
