// Package selfupdate implements ARTEX's one-click update from the web UI: pull a new binary from a GitHub
// Release, verify it, stage it, and atomically swap it in at the next startup.
//
// Division of labor (see start.sh / start.bat):
//
//	startup script = a simple supervisor loop whose only job is "decide whether to relaunch based on the exit code"
//	this package   = all the error-prone logic (download / SHA256 verification / smoke test / swap / rollback on failure)
//
// The swap lives in Go rather than in a script because the sha256 check and smoke test would need two separate
// implementations in sh and bat (sha256sum / shasum / certutil), and this is precisely the part that must not
// go wrong — install a binary that cannot run and the supervisor will faithfully relaunch it forever, leaving
// the user to rescue the machine by hand.
//
// A full upgrade goes through three process startups:
//
//	(1) old server receives /api/update/apply → download + verify → stage artex.new → exit 75
//	(2) script relaunches the old version → Bootstrap finds artex.new → verify + smoke test → swap → exit 75
//	(3) script relaunches, now the new version → Bootstrap records one attempt → clear marker after a successful start
//
// Any failure falls back to the old version: in (2), a failed verification deletes the staged file and keeps
// running the old version; in (3), crashing three times in a row before clearing the marker (i.e. unable to
// start) automatically swaps artex.old back in.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code meaning "please have the supervisor relaunch me" (EX_TEMPFAIL). The startup
// script sees it and immediately reruns, without counting it toward crash backoff. 0 means the user stopped
// normally (script exits its loop); everything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after a swap. Each startup of the new version
// increments the count; surviving settleDelay clears the marker. Crashing maxAttempts times in a row means the
// new version cannot start at all, so it is automatically rolled back.
const maxAttempts = 3

// Paths holds every file involved in one upgrade, all rooted in the **directory containing the executable**.
// We deliberately avoid the CWD: when running as a service the working directory may be / or any arbitrary path,
// and using the CWD would drop staged files elsewhere, breaking the swap logic outright.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // currently running binary        artex      / artex.exe
	New     string // staged new version            artex.new  / artex.new.exe
	Sum     string // sha256 of new version (hex)  artex.new.sha256 / artex.new.exe.sha256
	Old     string // old version backed up before swap  artex.old  / artex.old.exe
	Marker  string // upgrade status marker            artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows, .new/.old must also carry the .exe suffix or the smoke test and post-swap execution would fail, so
// we strip the extension first and then append it, keeping naming symmetric across platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of one swap so an automatic rollback can be triggered if the new version cannot start.
type marker struct {
	From     string `json:"from"`     // version before the upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // number of startup attempts since the swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes the staged files. It is called on a successful swap, a failed verification, or a user
// cancellation, so a leftover artex.new is never retried at the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version strings, returning -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version (for example a local development build "dev" or
// a git describe result like "0.3.7-2-gabc1234-dirty"); in that case callers should disable one-click update,
// otherwise a development build could be "upgraded" to a release and overwrite uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version of the form "v0.3.7" / "0.3.7" into [3]int.
//
// Only a clean three-part version is accepted: for non-tag builds build.sh uses git describe to produce suffixed
// versions like "0.3.7-2-gabc1234", which must be judged incomparable rather than treated as 0.3.7 — otherwise a
// development build could be mistaken for "already up to date" or be overwritten by a release build.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running inside a container. Under Docker the swap writes to the
// container's writable layer, and `docker compose up -d` recreating the container reverts to the version baked
// into the image — this is expected (the user is pulling a new image at that point), but the frontend needs to
// be able to explain it clearly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
