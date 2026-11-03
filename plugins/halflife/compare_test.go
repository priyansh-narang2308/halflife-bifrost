package halflife

import "testing"

type compareCase struct {
	claimed, observed string
	result            string
	order             int
}

func runCompare(t *testing.T, name string, fn func(string, string) Comparison, cases []compareCase) {
	t.Helper()
	for _, tc := range cases {
		got := fn(tc.claimed, tc.observed)
		if got.Result != tc.result || got.Order != tc.order {
			t.Errorf("%s(%q, %q) = %s order %d (%s), want %s order %d", name, tc.claimed, tc.observed, got.Result, got.Order, got.Detail, tc.result, tc.order)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	runCompare(t, "compareVersions", compareVersions, []compareCase{
		{"1.25", "Go 1.27.3", CompareDifferent, -1},
		{"Go 1.27", "1.27.3", CompareSame, 0},   // the claim names the release line
		{"1.27.3", "1.27", CompareDifferent, 1}, // a patch the observed value doesn't state counts as newer than .0
		{"v4.1.0", "4.1.0", CompareSame, 0},
		{"3.2.0", "v4.1.0 (latest)", CompareDifferent, -1},
		{"4.10", "4.9", CompareDifferent, 1}, // numeric, not lexical
		{"Python 3", "3.13.1", CompareSame, 0},
		{"4.1.0-rc.1", "4.1.0", CompareDifferent, -1}, // a pre-release precedes its release
		{"4.1.0", "4.1.0-rc.1", CompareDifferent, 1},
		{"4.1.0-rc.1", "4.1.0-rc.2", CompareDifferent, -1},
		{"latest", "4.1.0", CompareIncomparable, 0},
		{"", "", CompareIncomparable, 0},
	})
}

func TestComparePrices(t *testing.T) {
	runCompare(t, "comparePrices", comparePrices, []compareCase{
		{"$20 per month", "Pro plan: $29 per month, billed monthly.", CompareDifferent, -1},
		{"$29", "$29.00/mo", CompareSame, 0},                     // unstated period matches
		{"$1,049 per year", "1049 USD annually", CompareSame, 0}, // separators, codes, period words
		{"€10/mo", "10 euros a month", CompareSame, 0},
		{"$10 per month", "$100 per year", CompareIncomparable, 0}, // never converted
		{"$10", "€10", CompareIncomparable, 0},
		{"free", "$0", CompareSame, 0},
		{"free", "$5 per month", CompareDifferent, -1},
		{"$15 per user per month", "$15 per seat per month", CompareSame, 0},
		{"$0.15 per 1M input tokens", "$0.00015 per 1K tokens", CompareSame, 0},
		{"$0.15 per 1M tokens", "$0.15 per month", CompareIncomparable, 0},
		{"GPT-5 costs $1.25 per 1M tokens", "$1.25 per million tokens", CompareSame, 0}, // the amount beside the currency
		{"C$20", "$20", CompareIncomparable, 0},
		{"a lot", "$20", CompareIncomparable, 0},
	})
}

func TestCompareNames(t *testing.T) {
	runCompare(t, "compareNames", compareNames, []compareCase{
		{"Jordan Lee", "jordan lee", CompareSame, 0},
		{"Dr. Jordan Lee", "Jordan A. Lee Jr.", CompareSame, 0},
		{"Sam Altman", "Samuel Altman", CompareSame, 0},
		{"J. Lee", "Jordan Lee", CompareSame, 0},
		{"Lee, Jordan", "Jordan Lee", CompareSame, 0},
		{"José Núñez", "Jose Nunez", CompareSame, 0},
		{"Lee", "Jordan Lee", CompareSame, 0},
		{"Jordan Lee", "Dana Cruz", CompareDifferent, 0},
		{"Jordan Lee", "Morgan Lee", CompareDifferent, 0},
		{"Al Smith", "Alan Smith", CompareDifferent, 0}, // a two-letter prefix is too weak
		{"", "Dana Cruz", CompareIncomparable, 0},
	})
}

func TestCompareDates(t *testing.T) {
	runCompare(t, "compareDates", compareDates, []compareCase{
		{"2015", "founded in March 2015", CompareSame, 0},
		{"March 2015", "2015-04", CompareDifferent, -1},
		{"20 October 2026", "2026-10-20", CompareSame, 0},
		{"Oct 20, 2026", "20 Oct 2026", CompareSame, 0},
		{"20 Sept 2026", "2026-09-20", CompareSame, 0},
		{"11 August 2026", "2026-08-12", CompareDifferent, -1},
		{"2027", "2026", CompareDifferent, 1},
		{"soon", "2026", CompareIncomparable, 0},
	})
}

func TestCompareNumbers(t *testing.T) {
	runCompare(t, "compareNumbers", compareNumbers, []compareCase{
		{"1,200 employees", "1200", CompareSame, 0},
		{"1.2 million", "1,200,000 users", CompareSame, 0},
		{"12k", "12,500", CompareDifferent, -1},
		{"3.5%", "3.5 percent", CompareSame, 0},
		{"35%", "35", CompareIncomparable, 0},
		{"none", "12", CompareIncomparable, 0},
	})
}

func TestCompareValuesByCategory(t *testing.T) {
	for category, kind := range map[string]string{
		CategoryVersion: KindVersion, CategoryPrice: KindPrice, CategoryLeadership: KindName,
		CategoryEvent: KindDate, CategoryStatistic: KindNumber,
	} {
		if got, ok := compareValues(category, "1", "1"); !ok || got.Kind != kind {
			t.Errorf("%s: got %+v %v, want kind %s", category, got, ok, kind)
		}
	}
	for _, category := range []string{CategoryLaw, CategoryOther, ""} {
		if _, ok := compareValues(category, "1", "1"); ok {
			t.Errorf("%s has a comparator", category)
		}
	}
}
