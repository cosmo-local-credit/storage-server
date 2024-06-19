package api

import (
	"net/http"

	"github.com/VictoriaMetrics/metrics"
	"github.com/uptrace/bunrouter"
)

type metricsHandler struct {
	enable bool
}

func newMetricshandler(enable bool) *metricsHandler {
	return &metricsHandler{
		enable: enable,
	}
}

func (m *metricsHandler) metrics(w http.ResponseWriter, _ bunrouter.Request) error {
	if m.enable {
		metrics.WritePrometheus(w, true)
	}
	return nil
}
