package api

import (
	"github.com/VictoriaMetrics/metrics"
	"github.com/labstack/echo/v5"
)

type metricsHandler struct {
	enable bool
}

func newMetricshandler(enable bool) *metricsHandler {
	return &metricsHandler{
		enable: enable,
	}
}

func (m *metricsHandler) metrics(c *echo.Context) error {
	if m.enable {
		metrics.WritePrometheus(c.Response(), true)
	}
	return nil
}
