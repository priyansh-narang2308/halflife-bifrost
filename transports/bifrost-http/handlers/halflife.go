package handlers

import (
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/plugins/halflife"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// HalflifeReports is what the handler needs from the Halflife plugin.
type HalflifeReports interface {
	Report(id string) (halflife.Report, bool)
}

// HalflifeReportsResolver returns the loaded Halflife plugin, or nil. It is
// called per request, so plugin reloads are honoured (as for the cache).
type HalflifeReportsResolver func() HalflifeReports

// HalflifeHandler serves Halflife's audit reports. Its routes sit under /api
// and take the same middlewares, so they need the same dashboard auth.
type HalflifeHandler struct {
	resolve HalflifeReportsResolver
}

func NewHalflifeHandler(resolve HalflifeReportsResolver) *HalflifeHandler {
	return &HalflifeHandler{resolve: resolve}
}

func (h *HalflifeHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/halflife/reports/{id}", lib.ChainMiddlewares(h.getReport, middlewares...))
}

func (h *HalflifeHandler) getReport(ctx *fasthttp.RequestCtx) {
	plugin := h.resolve()
	if plugin == nil {
		SendError(ctx, fasthttp.StatusBadRequest, "halflife plugin is not loaded")
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
