package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/plugins/halflife"
	"github.com/valyala/fasthttp"
)

type fakeReports struct {
	reports map[string]halflife.Report
	list    []halflife.ReportSummary
	filter  halflife.ReportFilter // the last filter Reports was given
	metrics error
}

func (f *fakeReports) Report(id string) (halflife.Report, bool) { r, ok := f.reports[id]; return r, ok }

func (f *fakeReports) Reports(filter halflife.ReportFilter) []halflife.ReportSummary {
	f.filter = filter
	if filter.Limit < len(f.list) {
		return f.list[:filter.Limit]
	}
	return f.list
}

func (f *fakeReports) Usage() halflife.Usage { return halflife.Usage{Mode: halflife.ModeMonitor} }

func (f *fakeReports) WriteMetrics(w io.Writer) error {
	if f.metrics != nil {
		return f.metrics
	}
	_, err := io.WriteString(w, "halflife_queue_depth 0\n")
	return err
}

func handlerFor(f *fakeReports) *HalflifeHandler {
	return NewHalflifeHandler(func() HalflifeReports {
		if f == nil {
			return nil
		}
		return f
	})
}

func getHalflifeReport(h *HalflifeHandler, id string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.SetUserValue("id", id)
	h.getReport(ctx)
	return ctx
}

func call(handle fasthttp.RequestHandler, uri string) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.SetRequestURI(uri)
	handle(ctx)
	return ctx
}

func TestHalflifeReportFound(t *testing.T) {
	h := handlerFor(&fakeReports{reports: map[string]halflife.Report{
		"hlr_1": {ID: "hlr_1", RequestID: "req-1", Status: halflife.ReportComplete, Outcome: halflife.OutcomeEvidenceFound},
	}})
	ctx := getHalflifeReport(h, "hlr_1")
	var got halflife.Report
	if ctx.Response.StatusCode() != 200 || json.Unmarshal(ctx.Response.Body(), &got) != nil || got.RequestID != "req-1" || got.Outcome != "evidence_found" {
		t.Errorf("status %d body %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
}

func TestHalflifeReportMissing(t *testing.T) {
	if ctx := getHalflifeReport(handlerFor(&fakeReports{}), "hlr_x"); ctx.Response.StatusCode() != 404 {
		t.Errorf("unknown report: %d", ctx.Response.StatusCode())
	}
	h := handlerFor(nil)
	for name, handle := range map[string]fasthttp.RequestHandler{"report": h.getReport, "list": h.listReports, "usage": h.getUsage, "metrics": h.getMetrics} {
		if ctx := call(handle, "/"); ctx.Response.StatusCode() != 400 {
			t.Errorf("%s with the plugin not loaded: %d", name, ctx.Response.StatusCode())
		}
	}
}

func TestHalflifeReportList(t *testing.T) {
	f := &fakeReports{list: []halflife.ReportSummary{{ID: "hlr_3"}, {ID: "hlr_2"}, {ID: "hlr_1"}}}
	h := handlerFor(f)

	ctx := call(h.listReports, "/api/halflife/reports?tenant=vk-a&provider=openai&model=gpt-4o&verdict=CONTRADICTED&outcome=evidence_found&request_id=r1&before=hlr_9&limit=2")
	var page struct {
		Reports    []halflife.ReportSummary `json:"reports"`
		NextBefore string                   `json:"next_before"`
	}
	if ctx.Response.StatusCode() != 200 || json.Unmarshal(ctx.Response.Body(), &page) != nil {
		t.Fatalf("status %d body %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}
	want := halflife.ReportFilter{Tenant: "vk-a", Provider: "openai", Model: "gpt-4o", Verdict: "CONTRADICTED", Outcome: "evidence_found", RequestID: "r1", Before: "hlr_9", Limit: 2}
	if f.filter != want {
		t.Errorf("filter %+v, want %+v", f.filter, want)
	}
	if len(page.Reports) != 2 || page.NextBefore != "hlr_2" {
		t.Errorf("full page: %+v", page)
	}

	ctx = call(h.listReports, "/api/halflife/reports")
	if json.Unmarshal(ctx.Response.Body(), &page) != nil || len(page.Reports) != 3 || page.NextBefore != "" || f.filter.Limit != halflife.DefaultReportListLimit {
		t.Errorf("last page: %+v (limit %d)", page, f.filter.Limit)
	}

	for _, bad := range []string{"0", "-1", "x", "501"} {
		if ctx := call(h.listReports, "/api/halflife/reports?limit="+bad); ctx.Response.StatusCode() != 400 {
			t.Errorf("limit=%s: %d", bad, ctx.Response.StatusCode())
		}
	}

	if ctx := call(handlerFor(&fakeReports{}).listReports, "/api/halflife/reports"); !strings.Contains(string(ctx.Response.Body()), `"reports":[]`) {
		t.Errorf("empty list should be [], got %s", ctx.Response.Body())
	}
}

func TestHalflifeUsageAndMetrics(t *testing.T) {
	h := handlerFor(&fakeReports{})
	ctx := call(h.getUsage, "/api/halflife/usage")
	var u halflife.Usage
	if ctx.Response.StatusCode() != 200 || json.Unmarshal(ctx.Response.Body(), &u) != nil || u.Mode != halflife.ModeMonitor {
		t.Errorf("usage: %d %s", ctx.Response.StatusCode(), ctx.Response.Body())
	}

	ctx = call(h.getMetrics, "/api/halflife/metrics")
	if ctx.Response.StatusCode() != 200 || !strings.HasPrefix(string(ctx.Response.Header.ContentType()), "text/plain; version=0.0.4") || string(ctx.Response.Body()) != "halflife_queue_depth 0\n" {
		t.Errorf("metrics: %d %q %q", ctx.Response.StatusCode(), ctx.Response.Header.ContentType(), ctx.Response.Body())
	}

	ctx = call(handlerFor(&fakeReports{metrics: errors.New("boom")}).getMetrics, "/api/halflife/metrics")
	if ctx.Response.StatusCode() != 500 {
		t.Errorf("metrics write error: %d", ctx.Response.StatusCode())
	}
}
