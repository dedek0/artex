package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant in the package. WeCom limits by **bytes** and a
	// Chinese character takes 3 bytes, so any implementation that slices raw bytes would cut
	// a character in half, producing invalid UTF-8 that the platform rejects. Mixed
	// Chinese/English inputs of coprime lengths are used to hit every possible cut point.
	// NOTE: the CJK literals below are deliberate multibyte fixtures — they exercise the
	// 3-byte character-boundary path and must stay to keep this test meaningful.
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4-byte emoji; a wrong cut is even more obvious
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("input %q max=%d: produced invalid UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("input %q max=%d: result %d bytes exceeds the limit", in, max, len(got))
			}
			// Content must not be changed when it was not truncated.
			if len(in) <= max && got != in {
				t.Fatalf("input %q max=%d: content changed even though the limit was not exceeded -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should mean no limit")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should mean no limit")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// When max is smaller than the ellipsis itself, appending it must not push the result
	// over the limit.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("with max=1, result %q has length %d and exceeds the limit", got, len(got))
	}
	// Normally the ellipsis should be present.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("expected an ellipsis, got %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The metric difference from TruncateBytes must be preserved: Telegram limits by
	// characters; using a byte metric would cut a multibyte message down to a fraction.
	// NOTE: the CJK literal below is a deliberate fixture for this byte-vs-rune distinction.
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("expected 5 characters, got %d (%q)", n, got)
	}
	// The same string under a byte metric should be noticeably shorter.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("the byte metric should not produce the same character count as the rune metric")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("First line\n\nSecond line\twith a tab   multiple spaces", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("all whitespace should be collapsed, got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("consecutive spaces should not be preserved, got %q", got)
	}
	// After truncation the result must still be readable and valid.
	got = OneLine("abcdefghij", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("expected 4 characters, got %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Truncating HTML directly can cut a fragment like `<a href="htt`, and the platform
	// would reject the whole message.
	s := `<b>Title</b>Body body body<a href="https://example.com/very/long/path">View details</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: result %d characters exceeds the limit", max, n)
		}
		// The tail must not contain an unclosed `<` (a `<` in the last segment with no `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: tail tag was cut in half -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("no assets should return an empty string, got %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("all assets should be listed when under the limit, got %q", got)
	}
	// Over the limit the total count must be noted, otherwise readers cannot tell how many
	// assets were omitted.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "5 total") {
		t.Fatalf("total count 5 should be noted, got %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("an empty severity has ordinal 0 and should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("an empty threshold should let everything through")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("unknown status mapping, got %q", got)
	}
	// Unknown statuses are echoed as-is; no label is invented.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("unknown status should be echoed as-is, got %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers an omission found during review: truncation must
// avoid not only half-cut tags but also cut-off HTML entities.
//
// Once `&amp;` is cut to `&amp`, a parser that only recognizes entities may reject **the
// whole** message — and oversized digest messages are common, so the cost is too high.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain an entity fragment (an & with no matching ;).
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: entity fragment left at the tail %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: malformed entity appeared", max)
		}
	}
}
