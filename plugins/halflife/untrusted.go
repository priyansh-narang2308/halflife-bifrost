package halflife

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Untrusted web content (audit §4.3). Everything Tavily returns is written by
// whoever controls the page, so it is treated as data that may be hostile:
//
//   - sanitised before it is stored or shown to the judge: invisible and
//     control characters, markup, scripts and Markdown images removed;
//   - size-capped per field and per reply;
//   - kept only for http(s) URLs with a host;
//   - scanned for instruction-like text ("ignore previous instructions",
//     a fake verdict, chat-role markers). The sentence is removed and the
//     source flagged; a verdict cannot rest on a quote from a flagged source,
//     and a flagged source never counts towards evidence strength.
//
// None of this makes the judge safe on its own; it narrows what a page can
// do. The judge also has no tools, its reply must match a strict schema, and
// every quote it gives is checked against the sanitised text.

const (
	maxTitleBytes      = 300
	maxURLBytes        = 2048
	maxSourcesPerClaim = 20 // whatever a reply holds, at most this many sources are kept
	removedText        = "[removed]"
)

var (
	scriptBlockRe  = regexp.MustCompile(`(?is)<(script|style|noscript|template|iframe)\b[^>]*>.*?</\s*(script|style|noscript|template|iframe)\s*>`)
	htmlCommentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTagRe      = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9:-]*(\s[^<>]*)?/?>`)
	mdImageRe      = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	mdLinkRe       = regexp.MustCompile(`\[([^\]]*)\]\((?:[^)\s]*)(?:\s+"[^"]*")?\)`)
	spaceRunRe     = regexp.MustCompile(`[ \t\f\v]+`)
	blankLinesRe   = regexp.MustCompile(`\n\s*\n(\s*\n)+`)
	sentenceEndsRe = regexp.MustCompile(`[.!?]["')\]]*(\s|$)|\n`)
)

// instructionPatterns are signs that a page addresses the model reading it
// rather than its human readers. Each is narrow on purpose: a page about
// "ignoring the fine print" must not be flagged.
var instructionPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.\n]{0,30}\b(previous|prior|above|earlier|all|any|your|the system)\b[^.\n]{0,20}\b(instructions?|directions?|prompts?|rules)\b`), "asks to ignore instructions"},
	{regexp.MustCompile(`(?i)\b(new|updated|revised|additional)\s+(system\s+)?instructions?\s*:`), "gives new instructions"},
	{regexp.MustCompile(`(?i)\byou\s+are\s+(now\s+)?(an?\s+|the\s+)?(ai|assistant|language model|fact[- ]?checker|judge|verifier|chatbot)\b`), "addresses the model"},
	{regexp.MustCompile(`(?i)\b(system|developer)\s+prompt\b|\b(your|the model'?s?)\s+(system|developer)\s+(message|instructions)\b`), "mentions a system prompt"},
	{regexp.MustCompile(`(?i)"?\b(verdict|current_value|once_true|history_quote)\b"?\s*[:=]\s*"?(SUPPORTS|CONTRADICTS|CONFLICTING|INSUFFICIENT|CURRENT|OUTDATED|true|false)\b`), "contains a verdict"},
	{regexp.MustCompile(`(?i)\b(respond|reply|answer|output)\s+(only\s+)?(with|that)\b[^.\n]{0,30}\b(SUPPORTS|CONTRADICTS|INSUFFICIENT|CONFLICTING|verdict|json)\b`), "dictates the answer"},
	{regexp.MustCompile(`(?im)(<\|?im_(start|end)\|?>|\[/?INST\]|<<\s*/?SYS\s*>>|</?\s*(system|assistant|user)\s*>|^\s*(system|assistant)\s*:)`), "chat-role markup"},
}

// cleanWebText removes what a reader would not see or that only markup
// needs, keeping the visible text: invisible and control characters, HTML
// (with scripts and styles), Markdown images, and Markdown link targets.
func cleanWebText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ToValidUTF8(s, "")
	s = scriptBlockRe.ReplaceAllString(s, " ")
	s = htmlCommentRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = mdImageRe.ReplaceAllString(s, " ")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(s)
	s = strings.Map(visibleRune, s)
	s = spaceRunRe.ReplaceAllString(s, " ")
	s = blankLinesRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// visibleRune drops control, format (zero-width, bidirectional overrides),
// Unicode tag and private-use characters, which can hide text from a human
// reviewer while a model still reads it. Newlines and tabs stay.
func visibleRune(r rune) rune {
	switch {
	case r == '\n' || r == '\t':
		return r
	case r == '\r':
		return '\n'
	case r == utf8.RuneError,
		unicode.IsControl(r),
		unicode.Is(unicode.Cf, r), // format: U+200B–U+200F, U+202A–U+202E, U+2060–U+2069, U+FEFF, tags U+E0000–U+E007F
		unicode.Is(unicode.Co, r): // private use
		return -1
	case unicode.IsSpace(r):
		return ' '
	}
	return r
}

// flagged is the result of scanning one text for instruction-like content.
type flagged struct {
	text   string // the text with each offending sentence replaced by removedText
	reason string // the first pattern's reason; "" when nothing was found
}

// removeInstructions replaces every sentence that matches an instruction
// pattern with removedText.
func removeInstructions(s string) flagged {
	out := flagged{text: s}
	for {
		start, end, reason := firstInstruction(out.text)
		if start < 0 {
			return out
		}
		if out.reason == "" {
			out.reason = reason
		}
		from, to := sentenceAround(out.text, start, end)
		out.text = out.text[:from] + removedText + out.text[to:]
	}
}

func firstInstruction(s string) (int, int, string) {
	best, bestEnd, reason := -1, -1, ""
	for _, p := range instructionPatterns {
		if loc := p.re.FindStringIndex(s); loc != nil && (best < 0 || loc[0] < best) {
			best, bestEnd, reason = loc[0], loc[1], p.reason
		}
	}
	return best, bestEnd, reason
}

// sentenceAround widens [start, end) to the sentence(s) containing it.
func sentenceAround(s string, start, end int) (int, int) {
	from := 0
	for _, loc := range sentenceEndsRe.FindAllStringIndex(s[:start], -1) {
		from = loc[1]
	}
	to := len(s)
	if loc := sentenceEndsRe.FindStringIndex(s[end:]); loc != nil {
		to = end + loc[1]
	}
	// Keep the separator that ended the sentence, so neighbours stay apart.
	if to > from && to < len(s) && (s[to-1] == '\n' || s[to-1] == ' ' || s[to-1] == '\t') {
		to--
	}
	return from, to
}

// safeSourceURL returns u when it is an absolute http(s) URL with a host and
// within the size cap, else "".
func safeSourceURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" || len(u) > maxURLBytes {
		return ""
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Hostname() == "" || p.User != nil {
		return ""
	}
	return u
}

// untrustedSource turns one search result into a report source: URL checked,
// text cleaned and clipped, instruction-like sentences removed and flagged.
// ok is false when the source must be dropped (no usable URL).
func untrustedSource(r SearchResult) (ReportSource, bool) {
	u := safeSourceURL(r.URL)
	if u == "" {
		return ReportSource{}, false
	}
	title, _ := clip(cleanWebText(r.Title), maxTitleBytes)
	snippet, _ := clip(cleanWebText(r.Content), maxSnippetBytes*2) // clipped again after removal
	src := ReportSource{URL: u, Score: r.Score, PublishedDate: safeDate(r.PublishedDate)}
	t, s := removeInstructions(title), removeInstructions(snippet)
	src.Title = t.text
	src.Snippet, _ = clip(s.text, maxSnippetBytes)
	src.FlagReason = firstNonEmpty(s.reason, t.reason)
	src.Flagged = src.FlagReason != ""
	return src, true
}

// setExtracted cleans an Extract result onto the source, flagging it the
// same way.
func (s *ReportSource) setExtracted(raw string) {
	text, _ := clip(cleanWebText(raw), maxExtractedBytes*2)
	f := removeInstructions(text)
	s.Extracted, _ = clip(f.text, maxExtractedBytes)
	if f.reason != "" && !s.Flagged {
		s.Flagged, s.FlagReason = true, f.reason
	}
}

// safeDate keeps a published date only when it looks like one (Tavily sends
// RFC 1123 or ISO forms); anything else is page-controlled noise.
func safeDate(d string) string {
	d = strings.TrimSpace(d)
	if d == "" || len(d) > 40 {
		return ""
	}
	for _, r := range d {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" ,:+-./TZ", r)) {
			return ""
		}
	}
	return d
}

// answerDelimiterRe matches the tags the extraction prompt wraps the
// tenant's question and answer in.
var answerDelimiterRe = regexp.MustCompile(`(?i)</?\s*(answer|question)\b[^>]*>`)

// promptSafe removes our prompt delimiters from text that goes inside them,
// so the text can't close its own block.
func promptSafe(s string, delimiters *regexp.Regexp) string {
	return delimiters.ReplaceAllString(s, " ")
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
