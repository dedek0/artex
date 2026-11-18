package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// The HTTP surface for one-click page updates. The actual download/verify/swap logic lives in the
// selfupdate package; this layer only handles the auth boundary, mutual exclusion, progress broadcast,
// and telling main that 'it's time to exit'.
//
// The restart is not performed by this process: after staging the new version, the process exits with
// selfupdate.ExitRestart and is brought back up by the supervisor script (start.sh / start.bat, or the
// ENTRYPOINT under Docker).

// restartCh closes when an upgrade is ready or a rollback is complete; main exits with ExitRestart on receipt.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested returns a channel that closes when "please exit and let the supervisor bring me back up".
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState is the result of selfupdate.Bootstrap at this startup (upgrade succeeded / just rolled back /
// staged artifact discarded), injected by main so /api/update/check can tell the frontend honestly what
// happened to the last upgrade.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState is called once by main at startup.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache caches GitHub latest-release query results.
//
// The top bar's 'new version' hint checks on every full page load, while the unauthenticated GitHub
// API allows 60 requests per IP per hour — without caching, a few extra tabs or refreshes burn the
// quota, and a real update later cannot even query. An explicit 'check for updates' click can force a
// cache bypass.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch is the data-fetching function, an injection point for tests; nil means the real GitHub query.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// Cache failures briefly too, otherwise an unreachable GitHub makes every page load wait through a
	// timeout; but keep the TTL short so it self-heals quickly once the network recovers.
	releaseErrTTL = 2 * time.Minute
	// Timeout for version queries. NewClient's 30-minute timeout is for downloading the whole bundle;
	// a version check cannot wait that long.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get returns the latest Release, using the cache without network access on a hit.
//
// The lock is held for the whole fetch: concurrent callers queue for the one query's result instead
// of each hitting GitHub (several tabs checking at once right after page load is exactly when
// rate-limiting is most likely).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// A cancelled request (user closed the tab) does not mean GitHub is broken; do not cache it,
	// otherwise the next visitor gets a baffling "cancelled" error.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress is one progress update pushed to the frontend.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // meaningful only during download; -1 otherwise
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub holds the progress of one upgrade and broadcasts it to SSE subscribers.
//
// running doubles as a mutex: another POST /api/update/apply during an upgrade returns 409 directly,
// preventing two goroutines from writing to the same artex.new at once.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin claims the upgrade permission, returning false if one is already in progress.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "preparing…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish ends an upgrade. A nil err means staging succeeded and a restart is pending.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "update failed", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "new version ready, restarting…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout must be called while holding h.mu. Subscriber channels are buffered and drop on overflow —
// progress is disposable transient information and a stuck SSE connection must never block the upgrade.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck queries GitHub for the latest stable release and compares it with the current version.
//
// The frontend can also call api.github.com directly (GitHub's CORS is *), but THIS endpoint is
// authoritative: downloads are done by the backend, so an update is only possible if the backend can
// reach GitHub. It is common for the browser to connect while the server cannot (server on an internal
// network, or a proxy configured only in the browser); in that case clicking update is bound to fail,
// so it is better to report the error honestly at this check step.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// Online self-update is compiled out in this fork (selfupdate.Enabled).
	// Report "no update, disabled" and return before building a client or
	// querying any release feed, so this endpoint makes no outbound request.
	if !selfupdate.Enabled {
		out["disabled"] = true
		out["has_update"] = false
		out["comparable"] = false
		out["reason"] = "online updates are disabled in this build (no remote version check or download)"
		writeJSON(w, 200, out)
		return
	}

	// The top bar hint uses the cache by default; a user clicking "check for updates" sends force=1 to bypass it.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// Development builds (dev / git describe with a suffix) have no comparable version number. Allowing
		// an update would overwrite the locally debugged binary with a stable release, so no update is offered.
		out["reason"] = fmt.Sprintf("current version %q is not an official release; one-click update is disabled", current)
	}
	writeJSON(w, 200, out)
}

// updateApply downloads and stages a new version, then lets the process exit so the supervisor can
// restart it.
//
// It returns 202 immediately; the real work runs in a background goroutine: a full download can take
// minutes and would be cut off by a reverse proxy timeout if tied to the request. Progress is on
// /api/update/stream.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	// Online self-update is compiled out in this fork (selfupdate.Enabled):
	// refuse before any download so no remote binary is ever staged.
	if !selfupdate.Enabled {
		writeErr(w, 403, "online updates are disabled in this build")
		return
	}
	current := BuildVersion

	// Use the cache: ensure we install exactly the version the user saw and confirmed in the UI.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("current version %q is not an official release; one-click update is disabled", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("already on the latest version %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "an update is already in progress")
		return
	}

	go func() {
		// Deliberately use s.ctx rather than the request ctx: the request ends as soon as the HTTP response
		// is returned, and a download tied to it would be cancelled immediately.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] update failed: %v", err)
			return
		}
		log.Printf("[update] %s → %s staged, exiting to complete the swap", current, rel.TagName)
		// Give the last progress update a moment to reach the frontend before triggering exit.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback actively reverts to the previous version (the artex.old backup made before the swap).
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	// Part of the self-update machinery, which is compiled out in this fork
	// (selfupdate.Enabled). There is never a staged backup to roll back to,
	// so refuse rather than appear to offer a working control.
	if !selfupdate.Enabled {
		writeErr(w, 403, "online updates are disabled in this build")
		return
	}
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "an update is in progress, cannot roll back")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] manually rolled back to the previous version, exiting to complete the switch")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream pushes update progress over SSE.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Send the current state first so a page refresh immediately shows an in-progress upgrade.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
