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
	s.put(&Report{ID: "a", CompletedAt: &done, Claim: &ReportClaim{Text: "c"},
		Evidence: &ReportSources{Sources: []ReportSource{{URL: "https://one.example"}}}})
	got, _ := s.get("a")
	got.Claim.Text = "changed"
	got.Evidence.Sources[0].URL = "changed"
	*got.CompletedAt = time.Time{}
	again, _ := s.get("a")
	if again.Claim.Text != "c" || again.Evidence.Sources[0].URL != "https://one.example" || again.CompletedAt.IsZero() {
		t.Errorf("store shared memory with a reader: %+v", again)
	}
}
