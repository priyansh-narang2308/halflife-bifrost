package halflife

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Typed comparators: deterministic equality and order for the values claims
// assert (audit §3.4, point 4). The judge reads the current value from the
// evidence; these decide whether it is the claimed one, so a verdict on a
// version, price, name, date or count never rests on a model's say-so.

// Comparison results.
const (
	CompareSame         = "same"
	CompareDifferent    = "different"
	CompareIncomparable = "incomparable" // a value would not parse, or the two measure different things
)

// Comparator kinds.
const (
	KindVersion = "version"
	KindPrice   = "price"
	KindName    = "name"
	KindDate    = "date"
	KindNumber  = "number"
)

// Comparison is the outcome of comparing a claimed value with an observed one.
type Comparison struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	// Order is -1 when the claimed value is lower (older, cheaper, earlier)
	// than the observed one, 1 when higher, 0 when same or unordered.
	Order  int    `json:"order,omitempty"`
	Detail string `json:"detail"`
}

func incomparable(kind, format string, args ...any) Comparison {
	return Comparison{Kind: kind, Result: CompareIncomparable, Detail: fmt.Sprintf(format, args...)}
}

func ordered(kind string, order int, detail string) Comparison {
	if order == 0 {
		return Comparison{Kind: kind, Result: CompareSame, Detail: detail}
	}
	return Comparison{Kind: kind, Result: CompareDifferent, Order: order, Detail: detail}
}

// comparatorFor names the comparator for a claim category, or "" when the
// category has none and only the judge can decide.
func comparatorFor(category string) string {
	switch category {
	case CategoryVersion:
		return KindVersion
	case CategoryPrice:
		return KindPrice
	case CategoryLeadership:
		return KindName
	case CategoryEvent:
		return KindDate
	case CategoryStatistic:
		return KindNumber
	}
	return ""
}

// compareValues compares a claimed and an observed value with the
// comparator for category. ok is false when the category has none.
func compareValues(category, claimed, observed string) (Comparison, bool) {
	switch comparatorFor(category) {
	case KindVersion:
		return compareVersions(claimed, observed), true
	case KindPrice:
		return comparePrices(claimed, observed), true
	case KindName:
		return compareNames(claimed, observed), true
	case KindDate:
		return compareDates(claimed, observed), true
	case KindNumber:
		return compareNumbers(claimed, observed), true
	}
	return Comparison{}, false
}

// --- versions ---

// version is a dotted version: up to four numeric parts and a pre-release tag.
type version struct {
	parts      []int
	prerelease string
}

var versionRe = regexp.MustCompile(`(?i)\bv?(\d+(?:\.\d+){0,3})(?:-([0-9a-z][0-9a-z.]*))?\b`)

// parseVersion reads the first version in s: "Go 1.27.3", "v4.1.0", "3.2.0-rc.1".
// A bare integer counts ("Python 3"), so "version 4" parses as 4.
func parseVersion(s string) (version, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}
	var v version
	for _, p := range strings.Split(m[1], ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v.parts = append(v.parts, n)
	}
	v.prerelease = strings.ToLower(m[2])
	return v, true
}

func (v version) String() string {
	s := make([]string, len(v.parts))
	for i, p := range v.parts {
		s[i] = strconv.Itoa(p)
	}
	if v.prerelease != "" {
		return strings.Join(s, ".") + "-" + v.prerelease
	}
	return strings.Join(s, ".")
}

// compareVersions orders two versions over the parts the claim states:
// "1.25" is the same as "1.25.3", because a claim of the 1.25 line holds while
// 1.25.3 is current. A pre-release sorts before its release.
func compareVersions(claimed, observed string) Comparison {
	c, ok1 := parseVersion(claimed)
	o, ok2 := parseVersion(observed)
	if !ok1 || !ok2 {
		return incomparable(KindVersion, "no version in %q or %q", claimed, observed)
	}
	detail := c.String() + " vs " + o.String()
	for i, p := range c.parts {
		q := 0
		if i < len(o.parts) {
			q = o.parts[i]
		}
		if p != q {
			return ordered(KindVersion, sign(p-q), detail)
		}
	}
	switch {
	case c.prerelease == o.prerelease:
		return ordered(KindVersion, 0, detail)
	case c.prerelease == "" && len(o.parts) > len(c.parts):
		return ordered(KindVersion, 0, detail) // the claim names the release line the observed version is on
	case c.prerelease == "":
		return ordered(KindVersion, 1, detail)
	case o.prerelease == "":
		return ordered(KindVersion, -1, detail)
	}
	return ordered(KindVersion, sign(strings.Compare(c.prerelease, o.prerelease)), detail)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// --- prices ---

// price is an amount with what it is charged in and for. Empty fields are
// unstated, and unstated matches anything.
type price struct {
	amount   *big.Rat
	currency string // ISO 4217 code
	period   string // hour, day, week, month, year, once
	per      string // user, token (amount normalised to one million tokens)
}

var (
	currencySymbols = []struct{ symbol, code string }{
		{"us$", "USD"}, {"c$", "CAD"}, {"a$", "AUD"}, {"$", "USD"}, {"€", "EUR"}, {"£", "GBP"}, {"¥", "JPY"}, {"₹", "INR"},
	}
	currencyCodes = map[string]string{"usd": "USD", "eur": "EUR", "gbp": "GBP", "jpy": "JPY", "inr": "INR", "cad": "CAD", "aud": "AUD",
		"dollar": "USD", "dollars": "USD", "euro": "EUR", "euros": "EUR", "pound": "GBP", "pounds": "GBP"}
	amountRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?|\.\d+`)
	// An amount next to its currency, either side: "$1.25", "20 USD", "€ 10".
	pricedRe  = regexp.MustCompile(`(?i)(?:us\$|c\$|a\$|[$€£¥₹]|\b(?:usd|eur|gbp|jpy|inr|cad|aud)\b)\s?(\d[\d,]*(?:\.\d+)?|\.\d+)|(\d[\d,]*(?:\.\d+)?)\s?(?:usd|eur|gbp|jpy|inr|cad|aud|dollars?|euros?|pounds?)\b`)
	codeRe    = regexp.MustCompile(`(?i)\b(usd|eur|gbp|jpy|inr|cad|aud|dollars?|euros?|pounds?)\b`)
	freeRe    = regexp.MustCompile(`(?i)\b(free|no charge|no cost)\b`)
	periodRes = []struct {
		re     *regexp.Regexp
		period string
	}{
		{regexp.MustCompile(`(?i)(/\s*h(ou)?r\b|\bper hour\b|\ban hour\b|\bhourly\b)`), "hour"},
		{regexp.MustCompile(`(?i)(/\s*day\b|\bper day\b|\ba day\b|\bdaily\b)`), "day"},
		{regexp.MustCompile(`(?i)(/\s*w(ee)?k\b|\bper week\b|\ba week\b|\bweekly\b)`), "week"},
		{regexp.MustCompile(`(?i)(/\s*mo(nth)?\b|\bper month\b|\ba month\b|\bmonthly\b|\bmonth\b)`), "month"},
		{regexp.MustCompile(`(?i)(/\s*y(ea)?r\b|\bper year\b|\ba year\b|\bannual(ly)?\b|\byearly\b|\bper annum\b)`), "year"},
		{regexp.MustCompile(`(?i)\b(one-time|one time|lifetime)\b`), "once"},
	}
	perUserRe  = regexp.MustCompile(`(?i)\bper (user|seat|member)\b|/\s*(user|seat)\b`)
	perTokenRe = regexp.MustCompile(`(?i)(?:per|/)\s*(1\s*m|1m|million|1\s*k|1k|1,?000|thousand)?\s*(?:input |output )?tokens?\b`)
)

// parsePrice reads a price: "$49 per month", "€10/mo", "USD 20 per user per
// month", "$0.15 per 1M input tokens", "free".
func parsePrice(s string) (price, bool) {
	lower := strings.ToLower(s)
	var p price
	switch {
	case freeRe.MatchString(lower) && !amountRe.MatchString(lower):
		p.amount = new(big.Rat)
	default:
		// Prefer the amount beside a currency, so "GPT-5 costs $1.25" is 1.25.
		m := amountRe.FindString(lower)
		if pm := pricedRe.FindStringSubmatch(lower); pm != nil {
			m = pm[1] + pm[2]
		}
		if m == "" {
			return price{}, false
		}
		a, ok := new(big.Rat).SetString(strings.ReplaceAll(m, ",", ""))
		if !ok {
			return price{}, false
		}
		p.amount = a
	}
	for _, cs := range currencySymbols {
		if strings.Contains(lower, cs.symbol) {
			p.currency = cs.code
			break
		}
	}
	if p.currency == "" {
		if m := codeRe.FindString(lower); m != "" {
			p.currency = currencyCodes[strings.ToLower(m)]
		}
	}
	for _, pr := range periodRes {
		if pr.re.MatchString(lower) {
			p.period = pr.period
			break
		}
	}
	if m := perTokenRe.FindStringSubmatch(lower); m != nil {
		p.per = "token"
		basis := int64(1)
		switch strings.ReplaceAll(m[1], " ", "") {
		case "1m", "million":
			basis = 1_000_000
		case "1k", "1000", "1,000", "thousand":
			basis = 1_000
		}
		p.amount.Mul(p.amount, big.NewRat(1_000_000, basis)) // per million tokens
	} else if perUserRe.MatchString(lower) {
		p.per = "user"
	}
	return p, true
}

func (p price) String() string {
	s := p.amount.FloatString(4)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if p.currency != "" {
		s = p.currency + " " + s
	}
	if p.per == "token" {
		s += " per 1M tokens"
	} else if p.per != "" {
		s += " per " + p.per
	}
	if p.period != "" {
		s += " per " + p.period
	}
	return s
}

// comparePrices compares amounts when the two prices are charged in the
// same currency, for the same period and per the same unit (unstated
// matches anything). Monthly and yearly prices are never converted: annual
// billing is usually discounted, so the two are different offers.
func comparePrices(claimed, observed string) Comparison {
	c, ok1 := parsePrice(claimed)
	o, ok2 := parsePrice(observed)
	if !ok1 || !ok2 {
		return incomparable(KindPrice, "no price in %q or %q", claimed, observed)
	}
	detail := c.String() + " vs " + o.String()
	isFree := c.amount.Sign() == 0 || o.amount.Sign() == 0
	switch {
	case c.currency != "" && o.currency != "" && c.currency != o.currency && !isFree:
		return incomparable(KindPrice, "different currencies: %s", detail)
	case c.period != "" && o.period != "" && c.period != o.period:
		return incomparable(KindPrice, "different billing periods: %s", detail)
	case c.per != o.per && (c.per == "token" || o.per == "token"):
		return incomparable(KindPrice, "one is per token: %s", detail)
	}
	return ordered(KindPrice, c.amount.Cmp(o.amount), detail)
}

// --- person names ---

var (
	nameStop = map[string]bool{"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "sir": true, "dame": true,
		"jr": true, "sr": true, "ii": true, "iii": true, "phd": true}
	nameSplit = regexp.MustCompile(`[^\pL\pN]+`)
)

// nameTokens folds a person's name: diacritics, case, honorifics and
// suffixes removed; "Lee, Jordan" read as "Jordan Lee".
func nameTokens(s string) []string {
	if first, last, ok := strings.Cut(s, ","); ok && !strings.Contains(last, ",") && strings.TrimSpace(last) != "" {
		s = last + " " + first
	}
	folded, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	if err != nil {
		folded = s
	}
	var out []string
	for _, t := range nameSplit.Split(strings.ToLower(folded), -1) {
		if t != "" && !nameStop[t] {
			out = append(out, t)
		}
	}
	return out
}

// compareNames decides whether two names are the same person's: the same
// surname, and first names equal, one an initial of the other, or one a
// prefix of the other of at least three letters ("Sam" and "Samuel").
// Middle names are ignored. Names have no order.
func compareNames(claimed, observed string) Comparison {
	c, o := nameTokens(claimed), nameTokens(observed)
	if len(c) == 0 || len(o) == 0 {
		return incomparable(KindName, "no name in %q or %q", claimed, observed)
	}
	detail := strings.Join(c, " ") + " vs " + strings.Join(o, " ")
	if c[len(c)-1] != o[len(o)-1] {
		return Comparison{Kind: KindName, Result: CompareDifferent, Detail: detail}
	}
	if len(c) == 1 || len(o) == 1 {
		return Comparison{Kind: KindName, Result: CompareSame, Detail: detail + " (surname only)"}
	}
	if firstNamesMatch(c[0], o[0]) {
		return Comparison{Kind: KindName, Result: CompareSame, Detail: detail}
	}
	return Comparison{Kind: KindName, Result: CompareDifferent, Detail: detail}
}

func firstNamesMatch(a, b string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	switch {
	case a == b:
		return true
	case len(a) == 1:
		return b[0] == a[0]
	case len(a) >= 3:
		return strings.HasPrefix(b, a)
	}
	return false
}

// --- dates ---

// date is a calendar date known to a day, a month or a year.
type date struct {
	t         time.Time
	precision int // 1 year, 2 month, 3 day
}

var (
	monthWord = regexp.MustCompile(`(?i)\b(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?`)
	dateForms = []struct {
		re        *regexp.Regexp
		layout    string
		precision int
	}{
		{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), "2006-01-02", 3},
		{regexp.MustCompile(`\b\d{1,2} [A-Z][a-z]{2} \d{4}\b`), "2 Jan 2006", 3},
		{regexp.MustCompile(`\b[A-Z][a-z]{2} \d{1,2},? \d{4}\b`), "Jan 2 2006", 3},
		{regexp.MustCompile(`\b\d{4}-\d{2}\b`), "2006-01", 2},
		{regexp.MustCompile(`\b[A-Z][a-z]{2} \d{4}\b`), "Jan 2006", 2},
		{regexp.MustCompile(`\b(?:19|20)\d{2}\b`), "2006", 1},
	}
)

// parseDate reads the most precise date in s: "2026-10-20", "20 October
// 2026", "Oct 20, 2026", "October 2026", "2026".
func parseDate(s string) (date, bool) {
	// Month names become "Jan", "Feb", ... so one layout covers every spelling.
	s = monthWord.ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToUpper(m[:1]) + strings.ToLower(m[1:3])
	})
	for _, f := range dateForms {
		if m := f.re.FindString(s); m != "" {
			if t, err := time.Parse(f.layout, strings.ReplaceAll(m, ",", "")); err == nil {
				return date{t: t, precision: f.precision}, true
			}
		}
	}
	return date{}, false
}

// compareDates compares at the coarser of the two precisions: "2015" and
// "March 2015" are the same; "March 2015" and "April 2015" are not.
func compareDates(claimed, observed string) Comparison {
	c, ok1 := parseDate(claimed)
	o, ok2 := parseDate(observed)
	if !ok1 || !ok2 {
		return incomparable(KindDate, "no date in %q or %q", claimed, observed)
	}
	layout := map[int]string{1: "2006", 2: "2006-01", 3: "2006-01-02"}[min(c.precision, o.precision)]
	a, b := c.t.Format(layout), o.t.Format(layout)
	return ordered(KindDate, sign(strings.Compare(a, b)), a+" vs "+b)
}

// --- plain numbers ---

var (
	quantityRe = regexp.MustCompile(`(?i)(\d[\d,]*(?:\.\d+)?)\s*(%|percent\b|thousand\b|million\b|billion\b|trillion\b|k\b|m\b|bn\b)?`)
	scales     = map[string]int64{"thousand": 1e3, "k": 1e3, "million": 1e6, "m": 1e6, "billion": 1e9, "bn": 1e9, "trillion": 1e12}
)

// parseQuantity reads the first number in s with its scale ("1.2 million",
// "12k") and whether it is a percentage.
func parseQuantity(s string) (*big.Rat, bool, bool) {
	m := quantityRe.FindStringSubmatch(s)
	if m == nil {
		return nil, false, false
	}
	n, ok := new(big.Rat).SetString(strings.ReplaceAll(m[1], ",", ""))
	if !ok {
		return nil, false, false
	}
	unit := strings.ToLower(m[2])
	if f, ok := scales[unit]; ok {
		n.Mul(n, new(big.Rat).SetInt64(f))
	}
	return n, unit == "%" || unit == "percent", true
}

// compareNumbers compares counts and rates exactly; a percentage is never
// compared with a count.
func compareNumbers(claimed, observed string) Comparison {
	c, cPct, ok1 := parseQuantity(claimed)
	o, oPct, ok2 := parseQuantity(observed)
	if !ok1 || !ok2 {
		return incomparable(KindNumber, "no number in %q or %q", claimed, observed)
	}
	detail := c.RatString() + " vs " + o.RatString()
	if cPct != oPct {
		return incomparable(KindNumber, "a percentage and a count: %s", detail)
	}
	return ordered(KindNumber, c.Cmp(o), detail)
}

// statesValue reports whether text states value: verbatim, or as any value
// in text that the kind's comparator finds the same ("$29" in "from $20 to
// $29 per month"; "Dana Cruz" in "Dana Cruz, chief executive").
func statesValue(kind, value, text string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	if containsFold(text, value) {
		return true
	}
	if kind == KindName {
		// Each occurrence of the surname, read with the one or two words
		// before it as first and middle names. A full name needs its first
		// name there too: "Morgan Lee" does not state "Jordan Lee".
		want, have := nameTokens(value), nameTokens(text)
		if len(want) == 0 {
			return false
		}
		for i, t := range have {
			if t != want[len(want)-1] {
				continue
			}
			if len(want) == 1 {
				return true
			}
			for start := max(0, i-2); start < i; start++ {
				if compareNames(value, strings.Join(have[start:i+1], " ")).Result == CompareSame {
					return true
				}
			}
		}
		return false
	}
	var candidates []string
	switch kind {
	case KindVersion:
		candidates = versionRe.FindAllString(text, -1)
	case KindPrice:
		candidates = pricedRe.FindAllString(text, -1)
	case KindDate:
		normalised := monthWord.ReplaceAllStringFunc(text, func(m string) string { return strings.ToUpper(m[:1]) + strings.ToLower(m[1:3]) })
		for _, f := range dateForms {
			candidates = append(candidates, f.re.FindAllString(normalised, -1)...)
		}
	case KindNumber:
		candidates = quantityRe.FindAllString(text, -1)
	}
	for _, c := range candidates {
		cmp, _ := compareValues(categoryOf(kind), value, c)
		if cmp.Result == CompareSame {
			return true
		}
	}
	return false
}

// categoryOf is comparatorFor's inverse.
func categoryOf(kind string) string {
	return map[string]string{KindVersion: CategoryVersion, KindPrice: CategoryPrice, KindName: CategoryLeadership,
		KindDate: CategoryEvent, KindNumber: CategoryStatistic}[kind]
}
