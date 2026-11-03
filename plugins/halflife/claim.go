package halflife

import (
	"regexp"
	"strings"
	"unicode"
)

// Claim picking for the minimal verifier (M1): one sentence per answer, chosen
// by a keyword heuristic, never by a model. It looks for the kind of sentence
// that goes stale: a present-tense statement with a number, a date, a price or
// a version in it. The model-based extractor (Tasks 27-29) replaces it.

const (
	maxClaimBytes = 400 // one sentence; longer ones are cut
	maxQueryBytes = 380 // Tavily rejects very long queries; keep a margin
	minClaimWords = 4
	minClaimScore = 2
)

var (
	// Sentence ends: terminal punctuation followed by space, or a line break.
	sentenceEnd = regexp.MustCompile(`([.!?])\s+|\n+`)
	// Markdown list markers, headings, quotes and emphasis at a line start.
	markdownLead = regexp.MustCompile(`^\s*(?:[#>*\-+]+|\d+[.)])\s+`)
	// A number with an optional currency, unit or percent: 1.22, $49, 30%, 2026.
	number = regexp.MustCompile(`[$€£¥]?\d[\d,]*(?:\.\d+)?\s?%?`)

	temporalWords = []string{"latest", "current", "currently", "now", "today", "recent", "recently", "newest", "as of", "this year", "this month", "at the moment", "still"}
	factWords     = []string{" is ", " are ", " costs ", " cost ", " priced", " price", " released", " launched", " supports", " offers", " version", " ceo ", " president", " leads ", " runs ", " starts at", " per month", " per year"}
	hedgeWords    = []string{"i think", "i believe", "might", "may ", "could ", "perhaps", "probably", "in my opinion", "it depends"}
	leadPronouns  = []string{"it ", "this ", "that ", "they ", "these ", "those ", "its "}
)

// pickClaim returns the answer's most checkable sentence, or ok=false when no
// sentence scores high enough to be worth a search.
func pickClaim(answer string) (claim string, ok bool) {
	best, bestScore := "", 0
	for _, s := range sentences(answer) {
		if score := claimScore(s); score > bestScore { // ties keep the earlier sentence
			best, bestScore = s, score
		}
	}
	if bestScore < minClaimScore {
		return "", false
	}
	return best, true
}

// sentences splits text into cleaned sentences, dropping questions and
// fragments too short to state a fact.
func sentences(text string) []string {
	var out []string
	marked := sentenceEnd.ReplaceAllString(strings.ReplaceAll(text, "`", ""), "$1\x00")
	for _, raw := range strings.Split(marked, "\x00") {
		s := strings.TrimSpace(markdownLead.ReplaceAllString(raw, ""))
		s = strings.TrimSpace(strings.NewReplacer("**", "", "__", "").Replace(s))
		if s == "" || strings.HasSuffix(s, "?") || len(strings.Fields(s)) < minClaimWords {
			continue
		}
		if c, cut := clip(s, maxClaimBytes); cut {
			s = c
		}
		out = append(out, s)
	}
	return out
}

// claimScore rates how checkable, and how likely to go stale, a sentence is.
func claimScore(s string) int {
	lower := " " + strings.ToLower(s) + " "
	score := 2 * min(len(number.FindAllString(s, -1)), 3)
	if containsAny(lower, temporalWords) {
		score += 2
	}
	if containsAny(lower, factWords) {
		score++
	}
	if containsAny(lower, hedgeWords) {
		score -= 3 // a guess is not a claim, however many numbers it holds
	}
	if strings.HasPrefix(lower, " i ") || strings.HasPrefix(lower, " you ") {
		score-- // advice or narration, not a fact about the world
	}
	return score
}

// buildQuery turns a claim into a search query. A claim that opens with a
// pronoun ("It costs $49") means nothing on its own, so the question that
// named the subject goes in front of it.
func buildQuery(claim, question string) string {
	q := claim
	lower := strings.ToLower(claim)
	if question = strings.TrimSpace(question); question != "" && hasPrefixAny(lower, leadPronouns) {
		q = question + " " + claim
	}
	q = strings.Join(strings.FieldsFunc(q, func(r rune) bool { return unicode.IsSpace(r) }), " ")
	if c, cut := clip(q, maxQueryBytes); cut {
		q = c
	}
	return q
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
