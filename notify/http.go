package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets decides whether delivery to loopback / link-local addresses is
// allowed.
//
// Denied by default. These address ranges are not where IM bots or public mail servers
// live, but what they reach is sensitive: another service's admin port on the same host,
// and the cloud metadata endpoint (169.254.169.254, which exposes instance credentials).
// The delivery address is configured by an administrator, but an admin session hijacked
// via XSS/CSRF, or a second person sharing the same JWT, could edit the config to read
// back the response content — doJSON writes the first 200 bytes of a 4xx/5xx response
// body into last_error, and the delivery history endpoint echoes it, forming a semi-blind
// read primitive.
//
// However, a "local SMTP relay" (postfix on 127.0.0.1:25) is a common self-hosted mail
// configuration, and a blanket ban would block it. So an explicit escape hatch is left
// instead of a hard-coded allowance: set ARTEX_NOTIFY_ALLOW_LOCAL=1 to allow.
//
// It is exported as AllowLocalTargetsEnv so tests can explicitly turn it on — tests in
// this package and the server package make heavy use of httptest fake receivers on
// 127.0.0.1, which would all be blocked by the guard otherwise.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to a range "not allowed by
// default".
//
// Only loopback, link-local (including cloud metadata 169.254.169.254), unspecified and
// multicast are rejected. **RFC1918 private ranges are not rejected**: self-hosted
// Mattermost / SMTP relays on an internal network are a common, legitimate usage, and
// blocking them would make the feature unusable in real environments. This tradeoff is
// deliberate — the guard must block genuinely sensitive targets without breaking normal
// deployments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be converted back to IPv4 before checking,
	// otherwise it bypasses the guard.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is the Control hook of http.Transport's dialer, checking the target
// address **at connection setup**.
//
// Why it is at the dial stage rather than only at config-save time: this is the final
// enforcement point. It also covers two ways of bypassing the config check — DNS rebinding
// (resolving to a public IP at validation time but to an internal one at connect time) and
// redirects (although cross-host redirects are rejected, a same-host redirect can still
// point the path elsewhere).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("could not parse target address %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("refusing to deliver to a loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport adds just one dial guard on top of the default Transport. Clone keeps
// all the default tuning (connection pooling, HTTP/2, timeouts, proxy, etc.) so adding a
// single check does not change other behavior.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client shared by all channel deliveries.
//
// It deliberately does **not** reuse the project's global outbound proxy (the server
// side's GlobalProxy): that proxy is for penetration-test target traffic and is often an
// unstable tunnel, while notification availability should not be held hostage by target
// network jitter. IM pushes go direct. The timeout is 15 seconds — a peer slower than
// that is effectively in a failed state.
//
// Cross-host redirects are rejected: delivery addresses here are all "one fixed
// endpoint" and would not normally redirect to another host; the credentials for these
// platforms (DingTalk access_token, WeCom key, Telegram bot token) are **in the URL**, so
// following a cross-host redirect hands the credential to the redirect target. Same-host
// redirects (e.g. adding a trailing slash) are still allowed.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("refusing cross-host redirect (%s → %s)", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit bounds how much of the response body is read. A misbehaving peer can
// return an enormous body, while only the error code and a short description are needed
// for display in the delivery history.
const respBodyLimit = 8 << 10

// doJSON sends one request and returns the response body (length-limited).
//
// A nil payload sends an empty body (for GET or platforms that require no body). Keys and
// values in headers are attached verbatim, for generic Webhook custom headers.
//
// Error classification is this function's core responsibility: network-layer failures and
// 5xx/408/429 are "retryable", other 4xx are "permanent" — retrying a 403 just writes
// the same error to the log three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failure is a local bug (a config field has the wrong type);
			// retrying will not help.
			return nil, Permanent(fmt.Errorf("failed to build request body: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// An invalid URL — most likely the user mistyped the address — is a permanent
		// failure. Again, err must not be propagated: url.Parse's error text contains the
		// full address.
		return nil, Permanent(fmt.Errorf("invalid request URL: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout — mostly transient, left to backoff
		// retry.
		//
		// The error text must be redacted before being propagated. Reason: http.Client.Do
		// returns a *url.Error whose Error() is `Op "full URL": underlying error`, and the
		// credentials for these platforms are **in the URL** (DingTalk access_token, WeCom
		// key, Feishu hook id, Telegram /bot<token>/). Without redaction, the credential
		// flows through this error string to four places: notification_deliveries'
		// last_error (stored in plaintext), the delivery history endpoint's response
		// (**bypassing channel config masking**), server logs, and the 502 text returned
		// to the frontend by the test-send endpoint.
		return nil, fmt.Errorf("request failed: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("failed to read response: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limited) and 408 (timeout) are worth retrying; other 4xx are config or
	// permission problems where retrying is pointless.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("peer rate-limited or timed out (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("peer service error (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("peer rejected the request (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet flattens the response body into a short single line for error messages.
// Responses may contain newlines and lots of whitespace, which would wreck the layout of
// the delivery history page if put into last_error directly.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget compresses a delivery address to "scheme://host/…" for error
// messages.
//
// This is this package's only address-redaction policy, deliberately made **blunt**:
// everything except the scheme and host is discarded. The reason is that there is no
// "generic and safe" way to tell which part of a URL is a credential:
//
//	DingTalk   credential in query        /robot/send?access_token=xxx
//	WeCom      credential in query        /cgi-bin/webhook/send?key=xxx
//	Feishu     credential in **last path segment** /open-apis/bot/v2/hook/<hook_id>
//	Telegram   credential in **middle path segment** /bot<token>/sendMessage
//
// Trying to "keep only the useful parts" would require per-channel patches, and missing
// any one of them would be a credential leak. Keeping the host is enough for diagnosis
// (DNS resolution failures, connection failures, bad certificates can all be located);
// which bot it is can be recognized from the masked tail hint in the channel config.
//
// On a parse failure a fixed placeholder is returned — the original string is never
// echoed.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(address could not be parsed)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError strips the address from a transport-layer error, keeping only the
// underlying cause.
//
// A *url.Error is structured as {Op, URL, Err}, and its Error() prints the URL too. This
// explicitly reads the Err field, bypassing Error() — more reliable than doing string
// replacement afterwards, which would have to correctly handle every URL-encoded/escaped
// variant and is easy to get wrong.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: unknown error", uerr.Op, host)
	}
	// Non-*url.Error errors (e.g. those from the redirect policy) may also carry
	// addresses; route them through redaction too.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces any http(s) address in a piece of text with its redacted
// form.
//
// Used as a fallback for errors where structured fields are unavailable (redirect policy
// errors, custom errors from third-party libraries). Only http/https prefixes are
// recognized, split on whitespace and quotes — addresses do not contain those characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
