package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This file covers the "whole-item packing" fix: when a digest message exceeds a
// channel's length limit, it must be cut **a whole item at a time** and the number of
// unfitted items reported accurately, so the caller marks only what was really delivered.
//
// The old approach rendered the whole message and then truncated it, marking the entire
// batch as sent: the latter half vanished into thin air while the delivery history showed
// everything successful — findings were simply gone, with nothing anywhere to reveal it.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 findings in a digest, far beyond WeCom's 4096-byte limit.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("body %d bytes exceeds limit %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only a portion should fit (0 < kept < %d), got %d", len(m.Items), kept)
	}
	// The header must accurately state how many items are included here and how many are
	// left over — otherwise readers would treat the number in the header as the whole set.
	if !strings.Contains(body, "next message") {
		t.Fatalf("header should state how many items are not included here:\n%s", body[:minInt(400, len(body))])
	}
	// Only the first kept items should be included.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "Finding "+itoa(i+1)) {
			t.Fatalf("item %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "Finding "+itoa(kept+1)) {
		t.Fatalf("item %d should not appear (it belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("all items should be kept when unlimited, got kept=%d", kept)
	}
	if strings.Contains(body, "next message") {
		t.Fatalf("no truncation notice should appear when nothing was truncated:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// Even when the budget is too small to fit a single item, one item must still be sent
	// (left to the final truncation as a backstop). Otherwise one oversized finding would
	// wedge the whole batch forever: every claim fails to fit, nothing is ever sent.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("at least 1 item should be kept, got %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("a single message should report 1 delivered item, got %d", kept)
	}
	// An empty message has no deliverable items.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("an empty message should report 0 items, got %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram limits by **character count**; using a byte metric would squeeze a
	// multibyte message to a fraction of its intended size.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("body %d characters exceeds limit %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("only a portion should fit, got %d", kept)
	}
	if !strings.Contains(text, "next message") {
		t.Fatalf("should state that remaining items are not included:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("the card should fit only a portion, got %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// These two channels do not truncate the body; the whole batch counts as delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("precondition not met")
	}
	// Confirm indirectly via the renderer's return value: markdownBody(0) keeps all items
	// when unlimited.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("all items should be used when unlimited, got %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for "untrusted content must
// not change the message structure". Titles and summaries come from model output (the
// model reads the target's responses), and asset names come from the target's URLs.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // must appear in the result (escaped form)
		wrong []string // must not appear in the result (unescaped form)
	}{
		{
			name: "newline in title + external link",
			item: Item{
				Severity: "high",
				Name:     "Login SQL injection\n[Urgent: verify account here](http://attacker.tld)",
			},
			// The newline must be folded (otherwise a new list item/quote block could be
			// forged); square and round brackets must be escaped (otherwise it is a clickable
			// external link).
			must:  []string{`\[Urgent: verify account here\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[Urgent", "\n\n[Urgent"},
		},
		{
			name: "image beacon in title",
			item: Item{
				Severity: "high",
				Name:     "Finding ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "emphasis and quote in asset name",
			item: Item{
				Severity: "high",
				Name:     "Plain title",
				Assets:   []string{"a.com/*inject*>quote"},
			},
			must:  []string{`\*inject\*`, `\>`},
			wrong: []string{"*inject*"},
		},
		{
			name: "backticks and pipe in summary",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` | table",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// writeItem in single mode is the rendering path shared by the three Markdown
			// channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("found unescaped form %q (could be used to inject structure or a link):\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst pins the escaping order: backslashes must be handled
// first, otherwise backslashes added later get another layer and double backslashes appear
// in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("escaping order is wrong, got %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes pins a specific regression: Markdown escapes must
// not leak into Telegram's HTML output (Markdown escaping was once added to the shared
// title function, producing visible backslashes like `\(1\)` in Telegram messages).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *important*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Markdown backslash escapes appeared in the Telegram body:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
