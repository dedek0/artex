package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to at most max bytes, ensuring the result is valid UTF-8
// and does not cut a character in half.
//
// Why cutting on character boundaries is required: the WeCom group bot's markdown has
// a hard limit of 4096 **bytes** (not characters), and a Chinese character takes 3
// bytes. Slicing raw bytes would split a character in two and produce invalid UTF-8 —
// the platform would either reject the whole message or show replacement boxes. The
// approach here is to step back from the budget position to the nearest rune start byte
// (utf8.RuneStart detects continuation bytes 0b10xxxxxx).
//
// max<=0 means no limit. An ellipsis is appended after truncation unless max is too
// small to fit it.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the ellipsis: drop the ellipsis and just truncate, to avoid
		// exceeding max.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine flattens multi-line text into a single line: all whitespace is collapsed,
// then it is truncated by character count. It is used for the title line of IM messages
// — summaries often contain newlines, which would break the layout if inserted directly
// into a table/title. max<=0 means no length limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to at most max characters (not bytes), appending an ellipsis
// when truncated. max<=0 means no limit.
//
// The difference from TruncateBytes is the platform's metric: WeCom limits by bytes,
// Telegram by character count. Using the wrong metric does not raise an error — it only
// makes the message much shorter than intended (one Chinese character = 3 bytes, so
// byte-truncating at 4096 leaves only about 1365 characters), so both functions must be
// kept and selected per channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML truncates an HTML fragment by character count without leaving a half-cut
// tag.
//
// Truncating HTML directly by characters can produce fragments like `<a href="htt`;
// the platform parser would either reject the whole message or swallow the following
// body text as an attribute value. The approach here is: truncate by characters first,
// then check for an unclosed `<` at the tail and back off to before it.
//
// No tag balancing (completing </b>, etc.) is done: Telegram's HTML parser closes
// unclosed tags automatically, while implementing balancing ourselves would have to
// handle quotes in attributes, comments and self-closing tags — a disproportionate cost
// for the benefit.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment starting with `<` (a `<` appears with no `>` after it),
	// back off to before the `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// Likewise, back off if the tail is a cut-off HTML entity (e.g. `&amp;` cut to
	// `&amp`). An entity fragment can cause **the entire message** to be rejected by a
	// parser that only recognizes entities — oversized digest messages are common, and
	// losing the whole notification over one is not worth it.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount computes how many items fit **completely** within the budget, so digest
// messages can be packed a whole item at a time.
//
// Why pack whole items instead of rendering everything and then truncating: truncation
// makes the latter half of the items vanish into thin air, while their delivery records
// are still marked as sent — invisible in the message and in the delivery history, and
// the findings are simply gone. With whole-item packing, items that do not fit stay in
// the database for the next batch, and the kept value returned to the caller is the
// number of items this message actually delivered.
//
// Parameters: maxSize<=0 means no limit; reserve is the amount set aside for the message
// header/footer; size measures length (platforms differ: WeCom/DingTalk count bytes,
// Telegram counts characters — using the wrong metric does not raise an error, it just
// squeezes a Chinese message far below the limit); render turns item idx into its actual
// text — length varies with content and cannot be estimated.
//
// Returns at least 1 (as long as items remain). Even a single extremely long item must be
// sent and left to the caller's final truncation as a backstop; otherwise one oversized
// finding would wedge the whole batch in place forever.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are the two metrics used by packItemCount. They are named so
// call sites do not contain a bare func(s string) int closure, which would make it hard
// to tell at a glance which metric is in use.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine renders a list of assets as a single display line, omitting the excess
// beyond limit and noting the total count. A finding may anchor dozens of assets, and
// listing them all would blow up the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " (" + itoa(len(assets)) + " total)"
}

// itoa is a short alias for strconv.Itoa, used only for building display text, to avoid
// importing strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
