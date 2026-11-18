package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel implements the DingTalk custom bot.
//
// Platform characteristics (which determine the implementation choices here):
//   - A single bot is rate-limited to 20 messages/minute; excess messages are silently
//     dropped (HTTP may still return 200), so rate limiting must be done client-side,
//     see DefaultRatePerMin.
//   - Security settings offer three choices: signing / custom keyword / IP allowlist.
//     Signing is the only option that does not depend on message content, so only signing
//     is supported (a bare webhook with none of the three enabled is also supported).
//   - Both success and failure return HTTP 200; the errcode in the body distinguishes
//     them — not checking errcode would record failed deliveries as successful.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// The DingTalk Webhook URL contains the access_token and is itself a credential, so it is
// masked in full.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The destination is the DingTalk Webhook URL itself; changing it requires re-affirming
// the signing secret for the new address.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

// Send delivers one message. When there is a deep link and it is a single item, an
// ActionCard (with a button) is used; otherwise Markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk's Markdown body has no explicit byte limit, but a cap is still applied to
	// guard against abnormally large evidence fields.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "View details",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors inside 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse DingTalk response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 means signature verification failed, 310000 means keyword mismatch —
		// both are configuration errors that retries will not heal.
		return 0, Permanent(fmt.Errorf("DingTalk returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL appends timestamp and sign parameters to the webhook per the
// official signing rules.
//
// Rule: string-to-sign = timestamp + "\n" + secret, and the HMAC-SHA256 **key is also
// secret**; the result is base64-encoded and then URL-encoded. timestamp is in
// milliseconds. When secret is empty the hook is returned unchanged, to support bots
// without signing enabled.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Do not propagate err: url.Parse's error text contains the full address (including
		// the access_token).
		return "", fmt.Errorf("failed to parse Webhook URL: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL checks that the address is usable and the scheme supported, and
// performs an internal-network check on literal IP targets.
//
// Two points of care:
//
//  1. **Error messages must be redacted.** url.Parse returns a *url.Error whose Error()
//     contains the **full original address**, and these platforms' addresses embed
//     credentials (DingTalk access_token, WeCom key, Telegram bot token, Feishu hook id).
//     This used to `return err` directly, so an "invalid address format" error leaked the
//     credential into the test endpoint's 400 response, the last_error persisted on every
//     delivery, server logs, and the delivery history endpoint.
//
//  2. **Literal IPs are checked for internal addresses directly**, while domains are left
//     to the dial stage (blockInternalDial is the final enforcement point and also covers
//     DNS rebinding). The check here gives immediate feedback when saving the config rather
//     than waiting for the first delivery to fail.
//
// Restricting the scheme is defensive: file:///gopher:// and similar can make
// http.Client behave unexpectedly (already blocked by the scheme check, but there is no
// reason to leave that surface open).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("address could not be parsed (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http/https are supported, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host name")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("refusing to deliver to a loopback/link-local address %s (if you really need to deliver to a local service, set %s=1)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
