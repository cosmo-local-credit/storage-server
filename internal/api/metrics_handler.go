package api

import (
	"net/http"

	"github.com/VictoriaMetrics/metrics"
	"github.com/labstack/echo/v5"
)

// prometheusContentType is the text exposition format VictoriaMetrics writes.
const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

type metricsHandler struct{}

func newMetricsHandler() *metricsHandler {
	return &metricsHandler{}
}

// metrics is only routed when metrics are enabled, so it has nothing to check.
func (m *metricsHandler) metrics(c *echo.Context) error {
	c.Response().Header().Set(echo.HeaderContentType, prometheusContentType)
	c.Response().WriteHeader(http.StatusOK)
	metrics.WritePrometheus(c.Response(), true)
	return nil
}
