package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is the adapter for one notification channel. Implementations must be
// **stateless**: the same instance is reused concurrently by multiple channel
// configurations, and credentials are always passed in via cfg.
type Channel interface {
	// Kind returns the channel type identifier, which must match the registry key.
	Kind() string
	// Validate is called when saving a configuration to check required fields and
	// formats. The returned error is shown directly to the configurator, so its text
	// must say which field is missing rather than a generic "invalid configuration".
	Validate(cfg map[string]any) error
	// Send delivers one message and returns the **number of items actually delivered**
	// along with an error.
	//
	// Why return a count: every platform has a message length limit, and a digest
	// message that cannot fit the whole batch gets truncated. If the caller marked the
	// entire batch as delivered unconditionally, the truncated items would vanish —
	// invisible in the message and shown as successful in the delivery history, with
	// nothing anywhere to reveal the findings were never sent. By returning kept, the
	// caller marks only the first kept items and leaves the rest for the next batch.
	//
	// A non-nil error means delivery failed; *PermanentError means it should not be
	// retried. On failure kept is meaningless and must be ignored by the caller.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin returns the platform's officially recommended per-minute
	// delivery limit, used as the default rate limit for new channel instances.
	// Returning 0 means there is no known limit.
	DefaultRatePerMin() int
	// SecretKeys returns the names of config keys that hold credentials. When the API
	// echoes the configuration, the values of these keys are masked; on update, a masked
	// value means the stored value is preserved. Only the implementation knows which
	// fields are credentials (for WeCom the entire Webhook URL is the credential, while
	// for DingTalk it is only the secret), so this knowledge must come from the channel
	// and cannot be guessed by upper layers.
	SecretKeys() []string
	// DestinationKeys returns the names of config keys that determine "where messages
	// are delivered".
	//
	// Like SecretKeys, this is security-sensitive: the destination address and the
	// credentials are two independent sets of fields. If "change only the address while
	// keeping the credentials" were allowed, anyone able to edit a channel config could
	// send the real stored credentials to a server they control, rendering masking
	// meaningless. See PrepareConfigUpdate.
	DestinationKeys() []string
}

// registry is the channel registry. An explicit literal is used instead of init()
// self-registration so the full set of channels is visible in one place and omissions
// in new channels are caught at compile time rather than by runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get returns the channel implementation for a type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds returns all supported channel types, sorted lexicographically (for a stable UI dropdown).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retried: bad credentials,
// target rejection, malformed request body, etc. Retries only make sense for transient
// failures (network blips, rate limiting, peer 5xx); repeatedly retrying a permanent
// failure with backoff will neither succeed nor avoid burying the real error in retry logs.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as a permanent failure. It returns nil when err is nil, so it
// can be written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether the err chain carries a permanent-failure marker.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- Config-reading helpers ----
//
// Channel config comes from a JSONB column in the database; after encoding/json
// deserialization it is a map[string]any, with all numbers as float64 and arrays as
// []any. The helpers below unify this conversion and tolerate type deviations caused
// by users leaving fields blank in the UI (e.g. filling a port as a string).

// cfgString reads a string config value, trimming surrounding whitespace — copy-pasting
// from a web form easily introduces it.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt reads an integer config value, accepting both float64 (the JSON default) and
// string sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool reads a boolean config value, also accepting the strings "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings reads a string-array config value, trimming whitespace and dropping empty
// strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// A single string is also accepted, to support form submissions with just one value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap reads a string-map config value (e.g. custom HTTP headers), trimming keys and
// values and dropping empty keys.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
