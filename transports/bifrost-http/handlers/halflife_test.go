package handlers

import (
	"encoding/json"
	"testing"

	"github.com/maximhq/bifrost/plugins/halflife"
	"github.com/valyala/fasthttp"
)

type fakeReports map[string]halflife.Report

func (f fakeReports) Report(id string) (halflife.Report, bool) { r, ok := f[id]; return r, ok }

func getHalflifeReport(h *HalflifeHandler, id string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", id)
	h.getReport(ctx)
	return ctx
}

func TestHalflifeReportFound(t *testing.T) {
	h := NewHalflifeHandler(func() HalflifeReports {
		return fakeReports{"hlr_1": {ID: "hlr_1", RequestID: "req-1", Status: halflife.ReportComplete, Outcome: halflife.OutcomeEvidenceFound}}
	})
	ctx := getHalflifeReport(h, "hlr_1")
	var got halflife.Report
	if ctx.Response.StatusCode() != 200 || json.Unmarshal(ctx.Response.Body(), &got) != nil || got.RequestID != "req-1" || got.Outcome != "evidence_found" {
		t.Errorf("status %d body %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestHalflifeReportMissing(t *testing.T) {
	h := NewHalflifeHandler(func() HalflifeReports { return fakeReports{} })
	if ctx := getHalflifeReport(h, "hlr_x"); ctx.Response.StatusCode() != 404 {
		t.Errorf("unknown report: %d", ctx.Response.StatusCode())
	}
	h = NewHalflifeHandler(func() HalflifeReports { return nil })
	if ctx := getHalflifeReport(h, "hlr_x"); ctx.Response.StatusCode() != 400 {
		t.Errorf("plugin not loaded: %d", ctx.Response.StatusCode())
	}
}
