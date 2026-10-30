package api

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// pushPathPrefix is the public push heartbeat route; everything after it is the
// caller's push token.
const pushPathPrefix = "/api/v1/push/"

// redactLogPath strips secrets that travel as URL path segments so they never
// reach the logs. The push heartbeat token is a heartbeat credential carried in
// the path (…/push/{token}); logging the raw path would otherwise leak it at
// INFO on every single push. Only the token segment is masked — the route stays
// legible.
func redactLogPath(p string) string {
	if len(p) > len(pushPathPrefix) && strings.HasPrefix(p, pushPathPrefix) {
		return pushPathPrefix + "<redacted>"
	}
	return p
}

// requestLogger emits one structured log line per request. Probe endpoints
// (/healthz, /readyz) are logged at debug level to keep steady-state logs
// quiet under liveness polling.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			level := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				level = slog.LevelDebug
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", redactLogPath(r.URL.Path)),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("remote_ip", clientIP(r)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}

// recoverer converts panics into structured error logs plus a 500 response
// (when nothing has been written yet) instead of tearing down the server.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rvr := recover()
				if rvr == nil {
					return
				}
				if rvr == http.ErrAbortHandler { //nolint:errorlint // sentinel comparison per net/http docs
					panic(rvr)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.Any("panic", rvr),
					slog.String("method", r.Method),
					slog.String("path", redactLogPath(r.URL.Path)),
					slog.String("request_id", middleware.GetReqID(r.Context())),
					slog.String("stack", string(debug.Stack())),
				)
				if ww, ok := w.(middleware.WrapResponseWriter); !ok || ww.Status() == 0 {
					writeProblem(w, http.StatusInternalServerError, "Internal Server Error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
