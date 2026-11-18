package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compaction" mechanism introduced in norma v0.4.0, exposed
// as a platform experimental feature toggled by the user in system settings. It is mutually
// exclusive with built-in compaction: noaadapter.Enable is the only entry point, attaching at once
// a context takeover handler (Compactor), the Compress tool, and three resident prompt segments;
// not calling Enable means off (built-in compaction works as usual). The switch is resolved by the
// noaEnabledFn injected into each agent and read once per run, so toggling only affects runs
// started afterwards — no agent rebuild needed.

// enableNoa wires noa into opts when the resolver reports it enabled. archiveRoot is the
// persistence base directory for compressed originals (the global workDir; all agents land under
// <workDir>/noa, not scattered into task/intent dirs), and sessionID names the archive subdirectory
// beneath it (globally unique, so no conflict within the same base dir).
//
// noa is experimental: a wiring failure must not interrupt real tasks. On error, report via onWarn
// and fall back to built-in compaction. On success, clear opts.Compaction to avoid agentcore
// warning about "two context managers set at once".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("Failed to enable noa compaction, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but when both are set agentcore warns every time; clear it explicitly.
	opts.Compaction = nil
}
