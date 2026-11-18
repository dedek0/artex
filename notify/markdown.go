package notify

import (
	"fmt"
	"strings"
)

// This file holds message rendering shared by the "Markdown family" channels
// (DingTalk, WeCom). Feishu uses card JSON, Telegram uses HTML, and email uses HTML; each
// renders in its own adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A finding may
// anchor dozens of assets; listing them all would bloat the message without adding value
// — nobody reads the fourth domain onward in an IM message.
const maxAssetsShown = 3

// maxSummaryRunes is the number of characters a summary is compressed to. IM messages
// are a "hint to go view the details", not the report itself; the full content lives in
// the platform.
const maxSummaryRunes = 120

// markdownReservedBytes is reserved for the message header (summary line + severity
// distribution + possible truncation notice) and footer (platform link). When packing
// whole items, this is deducted from the budget so the header and footer are never cut
// off — if they were, readers could not even tell which batch this is or how many items
// are missing.
const markdownReservedBytes = 320

// markdownEscape escapes Markdown metacharacters.
//
// Why this is required: finding titles, summaries, classes and asset display names all
// come from **untrusted sources** — titles and summaries come from model output (the
// model reads the target's responses), and an asset's url is the full URL obtained by
// scanning (including a target-controlled query string). Without escaping, a finding
// titled
//
//	Login SQL injection\n[Urgent: verify account here](http://attacker.tld)
//
// would render as a **clickable external link** in a security engineer's DingTalk/Feishu;
// and `![](http://attacker.tld/beacon)` would be fetched by the client at render time,
// effectively reporting "this finding has been viewed" and leaking the reader's IP. Even
// harmless content can use injected bold or quote blocks to push a serious finding below
// the fold.
//
// The escape set covers the characters that change structure or create clickable
// elements in headings/links/emphasis/lists/quotes/strikethrough. `\` must be handled
// first, otherwise backslashes added later would be escaped again.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText flattens untrusted text to a single line and escapes it, for use in
// Markdown bodies. Single-lining is the other half of the work besides escaping: a
// newline by itself can forge a new list item or quote block, and escape characters
// cannot stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle returns the message title (the IM platform's title bar/card title);
// its content is the **unescaped original text**.
//
// Escaping is deliberately not done here: this title is shared by renderers in four
// contexts — Markdown body, Telegram HTML, Feishu card plain_text, and the generic
// Webhook's JSON plus the email subject. Each context has different escaping rules
// (Markdown escapes left in HTML produce visible backslashes; in JSON they pollute the
// data), so escaping must be handled by each output side; see writeItem / feishuItemLines
// / telegramEscape. Markdown escaping was once added to the shared function, and
// Telegram messages ended up with visible backslashes like `\(1\)`.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("Finding digest · %d items total", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "Finding notification"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody renders the message body and returns it along with the **number of items
// actually written**.
//
// The returned kept is the number of items this delivery really delivered; the caller
// uses it to mark only the first kept items as sent. Items blocked by the channel's
// length limit must be left for the next batch rather than being marked as sent along
// with the rest. This is exactly the source of "silent loss": the message is truncated,
// but the delivery records show everything as sent, with nothing anywhere to reveal that
// the latter half was never emitted.
//
// maxBytes<=0 means no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// A single message is sent even if it is oversized (left to the final truncation
		// as a backstop): partial information about a finding is better than sending
		// nothing at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[View all in platform](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro renders the beginning of a digest message: time window, item count
// and severity distribution. With these, recipients can judge whether the batch needs
// immediate attention without opening the platform.
//
// items is the set of items **actually fitted**, total is the batch's intended count.
// When they differ, the message must state "how many items are in the next message" —
// otherwise readers assume the number in the header is the whole story, while the items
// never sent do not exist anywhere in the UI.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**%d new findings in the last %d minutes**", total, m.WindowMinutes)
	} else {
		fmt.Fprintf(&b, "**%d new findings**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(showing the first %d items here; the remaining %d will continue in the next message)", len(items), extra)
	}
	// Show the distribution by severity so readers can see at a glance whether there are
	// Critical items. Only items **actually included in this message** are counted, so
	// "Critical 3" matches the items that can be counted below.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem renders a single finding item.
//
// prefix is the ordinal used in a digest list; with single=true the full version is
// rendered (including summary and deep link), while a digest list renders only a one-line
// summary — otherwise a 50-item digest would become a long document.
//
// All externally sourced content (title/class/assets/summary) goes through markdownText:
// single-lining + escaping. The deep link is built from the administrator-configured
// public_base_url, is not untrusted content, and must remain a clickable link, so it is
// emitted as-is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Digest mode: a single line, with compressed assets and summary appended.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**Status change**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**Type**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**Assets**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**Summary**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[View details](%s)\n", it.DetailURL)
	}
}
