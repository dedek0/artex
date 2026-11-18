package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This file covers two related hardening measures:
//   ① Delivery addresses must not turn the server into a pivot into internal networks /
//      cloud metadata (SSRF)
//   ② Address-validation errors must not leak credentials from the address
//
// About the test environment: many cases in this package use httptest fake receivers on
// 127.0.0.1, which the guard blocks by default. TestMain therefore turns on
// AllowLocalTargetsEnv globally, while each SSRF case below explicitly clears it to assert
// the **deny-by-default** behavior.

func TestMain(m *testing.M) {
	// Let regular cases reach local fake receivers; SSRF cases clear it themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core SSRF assertion: under the default
// configuration, delivery to a loopback address must be refused **at the connection
// layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // close the escape hatch = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("loopback delivery should not be allowed by default")
	}
	if hit {
		t.Fatal("the request reached the local service — the guard did not work")
	}
	// The error should tell users how to allow it (a local SMTP relay is a legitimate
	// configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal should explain how to explicitly allow it: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn is the counter-case: once explicitly allowed it
// must work, otherwise legitimate deployments like local postfix / internal relays would
// be blanket-banned.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("delivery should work once explicitly allowed: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // cloud metadata endpoint — the main reason this function exists
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped form must be reduced first, or it is a bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be rejected", s)
		}
	}
	// RFC1918 private ranges are **deliberately allowed**: self-hosted Mattermost / SMTP
	// relays on an internal network are a common legitimate usage. This assertion pins the
	// tradeoff — if someone casually adds a private-range check later, it will fail here,
	// forcing a conscious decision instead of silently breaking a batch of deployments.
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s should be allowed (private ranges are common legitimate delivery targets)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets covers the early feedback at config time:
// a literal IP should be rejected at save time, not on the first failed delivery.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at config time", raw)
		}
	}
	// Public and private addresses pass as usual (private ranges are left to the dial stage,
	// which does not block them).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s should pass validation: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials covers a branch I missed in the previous
// round, found during review.
//
// When url.Parse **fails** it returns a *url.Error whose Error() contains the full original
// address. I had only redacted the error from http.Client.Do and missed this; the
// "permanent failure path" cases added at the time (file://, gopher://, ftp://) all parse
// successfully and take the scheme branch, so an all-green run proved nothing about this
// path — a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // illegal percent escape
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // non-numeric port
		"http://[::1?access_token=" + leakProbeToken,                  // unmatched bracket
	}
	for _, raw := range cases {
		// First confirm this input **really** makes url.Parse fail. Without this, the case
		// could silently wander into a different branch (that is where the previous round's
		// false guarantee came from).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q should fail to parse, otherwise this case does not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q should fail validation", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm the channel layer's wrapping does not leak the address either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("an invalid address should fail validation")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault covers the SMTP channel's dial guard.
//
// The email channel once used a bare net.Dialer, the only gap in the whole SSRF defense:
// setting host to 169.254.169.254 or 127.0.0.1 connected directly, and when
// smtp.NewClient's handshake failed it wrapped the peer's response line into an error echoed
// by the delivery history via last_error — the semi-blind read primitive already closed in
// the other channels; the timing difference between "connection refused" and "timeout"
// could also probe ports.
//
// This package's TestMain turns on AllowLocalTargetsEnv globally (many cases use fake
// receivers on 127.0.0.1), so this case must clear it itself — otherwise it would pass with
// or without the guard, which is exactly why the gap went undetected by any test.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // close the escape hatch = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("email delivery to a loopback address should not be allowed by default")
	}
	// The connection should never be established: the guard stops it in the Control hook and
	// EHLO is never sent.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("the SMTP session was established — the guard did not work")
	}
	// The error should tell users how to allow it (a local postfix relay is a legitimate
	// configuration).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("the refusal should explain how to explicitly allow it: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the paired counter-case: once explicitly
// allowed, delivery must work. Self-hosted internal SMTP / local relays are very common
// deployments; the guard must not ban them indiscriminately.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("local SMTP should deliver once explicitly allowed: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("no EHLO seen — the session was not really established")
	}
}
