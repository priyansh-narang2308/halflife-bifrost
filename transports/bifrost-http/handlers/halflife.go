package handlers

import (
	"bytes"
	"io"
	"strconv"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/halflife"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// HalflifeReports is what the handler needs from the Halflife plugin.
type HalflifeReports interface {
	Report(id string) (halflife.Report, bool)
	Reports(f halflife.ReportFilter) []halflife.ReportSummary
	Usage() halflife.Usage
	WriteMetrics(w io.Writer) error
}

// HalflifeReportsResolver returns the loaded Halflife plugin, or nil. It is
// called per request, so plugin reloads are honoured (as for the cache).
type HalflifeReportsResolver func() HalflifeReports

// HalflifeHandler serves Halflife's audit reports, per-tenant usage and
// Prometheus metrics. Its routes sit under /api and take the same
// middlewares, so they need the same dashboard auth.
type HalflifeHandler struct {
	resolve HalflifeReportsResolver
}

func NewHalflifeHandler(resolve HalflifeReportsResolver) *HalflifeHandler {
	return &HalflifeHandler{resolve: resolve}
}

func (h *HalflifeHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/halflife/reports", lib.ChainMiddlewares(h.listReports, middlewares...))
	r.GET("/api/halflife/reports/{id}", lib.ChainMiddlewares(h.getReport, middlewares...))
	r.GET("/api/halflife/usage", lib.ChainMiddlewares(h.getUsage, middlewares...))
	r.GET("/api/halflife/metrics", lib.ChainMiddlewares(h.getMetrics, middlewares...))
}

// plugin returns the loaded plugin, or answers 400 and returns nil.
func (h *HalflifeHandler) plugin(ctx *fasthttp.RequestCtx) HalflifeReports {
	p := h.resolve()
	if p == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "halflife plugin is not loaded")
	}
	return p
}

func (h *HalflifeHandler) getReport(ctx *fasthttp.RequestCtx) {
	plugin := h.plugin(ctx)
	if plugin == nil {
		return
	}
	id, _ := ctx.UserValue("id").(string)
	report, ok := plugin.Report(id)
	if !ok {
		SendError(ctx, fasthttp.StatusNotFound, "report not found (unknown, or evicted from the in-memory store)")
		return
	}
	SendJSON(ctx, report)
}

// listReports answers GET /api/halflife/reports?tenant=&provider=&model=
// &verdict=&outcome=&request_id=&before=&limit=, newest first. The next page
// is ?before=<last id>.
func (h *HalflifeHandler) listReports(ctx *fasthttp.RequestCtx) {
	plugin := h.plugin(ctx)
	if plugin == nil {
		return
	}
	args := ctx.QueryArgs()
	f := halflife.ReportFilter{
		Tenant:    string(args.Peek("tenant")),
		Provider:  string(args.Peek("provider")),
		Model:     string(args.Peek("model")),
		Verdict:   string(args.Peek("verdict")),
		Outcome:   string(args.Peek("outcome")),
		RequestID: string(args.Peek("request_id")),
		Before:    string(args.Peek("before")),
	}
	f.Limit = halflife.DefaultReportListLimit
	if v := args.Peek("limit"); len(v) > 0 {
		n, err := strconv.Atoi(string(v))
		if err != nil || n < 1 || n > halflife.MaxReportListLimit {
			SendError(ctx, fasthttp.StatusBadRequest, "limit must be an integer from 1 to "+strconv.Itoa(halflife.MaxReportListLimit))
			return
		}
		f.Limit = n
	}
	reports := plugin.Reports(f)
	if reports == nil {
		reports = []halflife.ReportSummary{}
	}
	next := "" // a full page may have more after it
	if len(reports) == f.Limit {
		next = reports[len(reports)-1].ID
	}
	SendJSON(ctx, map[string]any{"reports": reports, "next_before": next})
}

func (h *HalflifeHandler) getUsage(ctx *fasthttp.RequestCtx) {
	if plugin := h.plugin(ctx); plugin != nil {
		SendJSON(ctx, plugin.Usage())
	}
}

// getMetrics serves the Prometheus text exposition format (0.0.4).
func (h *HalflifeHandler) getMetrics(ctx *fasthttp.RequestCtx) {
	plugin := h.plugin(ctx)
	if plugin == nil {
		return
	}
	var buf bytes.Buffer
	if err := plugin.WriteMetrics(&buf); err != nil {
		SendError(ctx, fasthttp.StatusInternalServerError, "writing metrics: "+err.Error())
		return
	}
	ctx.SetContentType("text/plain; version=0.0.4; charset=utf-8")
	ctx.SetBody(buf.Bytes())
}
