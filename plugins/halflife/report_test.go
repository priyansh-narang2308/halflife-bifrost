package halflife

import (
	"fmt"
	"testing"
	"time"
)

func TestReportStoreEvictsOldest(t *testing.T) {
	s := newReportStore(3)
	for i := range 5 {
		s.put(&Report{ID: fmt.Sprint(i)})
	}
	if s.len() != 3 {
		t.Fatalf("len = %d", s.len())
	}
	for i, want := range []bool{false, false, true, true, true} {
		if _, ok := s.get(fmt.Sprint(i)); ok != want {
			t.Errorf("report %d held = %t, want %t", i, ok, want)
		}
	}
	if s.update("0", func(*Report) {}) {
		t.Error("updated an evicted report")
	}
	s.put(&Report{ID: "4", Status: ReportRunning}) // replacing keeps one entry
	if r, _ := s.get("4"); s.len() != 3 || r.Status != ReportRunning {
		t.Errorf("replace: len %d status %q", s.len(), r.Status)
	}
	s.remove("4")
	if _, ok := s.get("4"); ok || s.len() != 2 {
		t.Error("remove did not remove")
	}
}

// get returns a copy: changing it, or the store's report later, never shows
// through the other.
func TestReportStoreReturnsCopies(t *testing.T) {
	s := newReportStore(2)
	done := time.Now()
	h := 24
	s.put(&Report{ID: "a", CompletedAt: &done, Claims: []ReportClaim{{Text: "c",
		Search:   &SearchRequest{IncludeDomains: []string{"one.example"}, MaxAgeHours: &h},
		Evidence: &ReportSources{Sources: []ReportSource{{URL: "https://one.example"}}}}},
		Extraction: &LLMResult{GatewayRequestIDs: []string{"gw-1"}}})
	got, _ := s.get("a")
	got.Claims[0].Text = "changed"
	got.Claims[0].Evidence.Sources[0].URL = "changed"
	got.Claims[0].Search.IncludeDomains[0] = "changed"
	*got.Claims[0].Search.MaxAgeHours = 0
	got.Extraction.GatewayRequestIDs[0] = "changed"
	again, _ := s.get("a")
	c := again.Claims[0]
	if c.Text != "c" || c.Evidence.Sources[0].URL != "https://one.example" || c.Search.IncludeDomains[0] != "one.example" ||
		*c.Search.MaxAgeHours != 24 || again.Extraction.GatewayRequestIDs[0] != "gw-1" || again.CompletedAt.IsZero() {
		t.Errorf("store shared memory with a reader: %+v", again)
	}
}
