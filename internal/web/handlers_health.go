package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// HealthCheck is a dependency probe reported by /healthz. A nil error means healthy.
type HealthCheck func(ctx context.Context) error

// healthCheckTimeout bounds every probe so a hanging dependency cannot hang the endpoint.
const healthCheckTimeout = 3 * time.Second

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// HandleHealth serves the unauthenticated /healthz liveness/readiness probe.
//
// It reports the process as healthy (200) only when every registered probe succeeds, and
// as unavailable (503) otherwise, so container orchestrators and load balancers can gate
// traffic without an authenticated session. When no probe is registered the process is
// considered healthy, which keeps the endpoint usable in stripped-down deployments.
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	resp := healthResponse{Status: "ok", Checks: map[string]string{}}

	if s != nil && s.healthCheck != nil {
		probeCtx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		err := s.healthCheck(probeCtx)
		cancel()
		if err != nil {
			status = http.StatusServiceUnavailable
			resp.Status = "unavailable"
			resp.Checks["database"] = err.Error()
			slog.WarnContext(r.Context(), "health_check_failed",
				slog.String("check", "database"),
				slog.String("error", err.Error()),
			)
		} else {
			resp.Checks["database"] = "ok"
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
