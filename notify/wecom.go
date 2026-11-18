package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard limit on the WeCom group bot's markdown content (in
// bytes, not characters). It is the tightest limit of all six channels and the main
// reason TruncateBytes exists.
const weComMarkdownLimit = 4096

// weComChannel implements the WeCom group bot.
//
// Platform characteristics:
//   - Authenticated solely via the key in the URL; signing is not supported — so the
//     webhook URL itself is the entire credential.
//   - The markdown content limit is 4096 **bytes**; an oversized message is rejected in
//     full (not truncated). At 3 bytes per Chinese character, only a little over a
//     thousand characters fit, so client-side truncation is required.
//   - Rate-limited to 20 messages/minute, likewise handled client-side.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom has a single credential field (the webhook, i.e. the key in the URL) and does not
// support signing — the whole address is the entire credential, so there are no other
// fields to mask.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom has only the webhook field, which is both destination and credential, so there
// is no notion of a "credential left behind after an address change".
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("missing Webhook URL")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("invalid Webhook URL: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// A digest batch can be very long (50 items × one line plus prefix), easily exceeding
	// 4096 bytes. Truncation is done here rather than relying on a platform error: rejection
	// means the whole batch is lost, while truncation at least delivers the first several
	// items.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("failed to parse WeCom response: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 means the API rate limit was exceeded — the platform's window rolls, so
		// retrying with backoff works, and it is explicitly classified as retryable.
		// Reaching here means the client's rate_per_min is too aggressive; retry is only a
		// backstop, the real fix is to lower this channel's rate limit.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom rate limit %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 means the webhook key is invalid — a permanent failure that retries will
		// not heal.
		return 0, Permanent(fmt.Errorf("WeCom returned error %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
