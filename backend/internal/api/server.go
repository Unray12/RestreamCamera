// Package api exposes the REST API and serves the embedded web UI.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aclab/restreamcamera/internal/config"
	"github.com/aclab/restreamcamera/internal/cryptobox"
	"github.com/aclab/restreamcamera/internal/mediamtx"
	"github.com/aclab/restreamcamera/internal/provision"
	"github.com/aclab/restreamcamera/internal/store"
	"github.com/aclab/restreamcamera/internal/telemetry"
	"github.com/aclab/restreamcamera/internal/webui"
)

// Server wires HTTP handlers to the application services.
type Server struct {
	cfg     *config.Config
	st      *store.Store
	mtx     *mediamtx.Client
	box     *cryptobox.Box
	rec     *provision.Reconciler
	tel     *telemetry.Collector
	log     *slog.Logger
	runs    *runStore
	started time.Time
}

// New creates the API server.
func New(cfg *config.Config, st *store.Store, mtx *mediamtx.Client, box *cryptobox.Box,
	rec *provision.Reconciler, log *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		st:      st,
		mtx:     mtx,
		box:     box,
		rec:     rec,
		log:     log,
		runs:    newRunStore(),
		started: time.Now(),
	}
}

// Handler builds the full HTTP handler including auth middleware and UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// public endpoints
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/mediamtx/auth", s.handleMediaAuth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/healthz", s.handleHealth)

	// authenticated endpoints
	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/samples", s.handleSamples)
	mux.HandleFunc("GET /api/v1/system", s.handleSystem)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/monitor/stream", s.handleMonitorStream)
	mux.HandleFunc("GET /api/v1/monitor/overview", s.handleMonitorOverview)
	mux.HandleFunc("GET /api/v1/monitor/series", s.handleMonitorSeries)
	mux.HandleFunc("GET /api/v1/monitor/cameras", s.handleMonitorCameras)
	mux.HandleFunc("GET /api/v1/monitor/cameras/{id}", s.handleMonitorCamera)

	mux.HandleFunc("GET /api/v1/sources", s.handleListSources)
	mux.HandleFunc("POST /api/v1/sources", s.handleCreateSource)
	mux.HandleFunc("GET /api/v1/sources/{id}", s.handleGetSource)
	mux.HandleFunc("PATCH /api/v1/sources/{id}", s.handlePatchSource)
	mux.HandleFunc("DELETE /api/v1/sources/{id}", s.handleDeleteSource)
	mux.HandleFunc("POST /api/v1/sources/{id}/test", s.handleTestSource)

	mux.HandleFunc("GET /api/v1/cameras", s.handleListCameras)
	mux.HandleFunc("POST /api/v1/cameras", s.handleCreateCameras)
	mux.HandleFunc("POST /api/v1/cameras/bulk", s.handleCreateCameras)
	mux.HandleFunc("POST /api/v1/cameras/bulk-action", s.handleBulkAction)
	mux.HandleFunc("GET /api/v1/cameras/export", s.handleExportCameras)
	mux.HandleFunc("GET /api/v1/cameras/{id}", s.handleGetCamera)
	mux.HandleFunc("PATCH /api/v1/cameras/{id}", s.handlePatchCamera)
	mux.HandleFunc("DELETE /api/v1/cameras/{id}", s.handleDeleteCamera)
	mux.HandleFunc("POST /api/v1/cameras/{id}/actions/{action}", s.handleCameraAction)
	mux.HandleFunc("GET /api/v1/cameras/{id}/preview", s.handleCameraPreview)

	mux.HandleFunc("POST /api/v1/loadtest", s.handleStartLoadtest)
	mux.HandleFunc("GET /api/v1/loadtest", s.handleListLoadtests)
	mux.HandleFunc("POST /api/v1/loadtest/{id}/stop", s.handleStopLoadtest)
	mux.HandleFunc("GET /api/v1/loadtest/{id}", s.handleGetLoadtest)
	mux.HandleFunc("GET /api/v1/metrics", s.handleMetrics)

	// static UI (must be last)
	mux.Handle("/", webui.Handler())

	return s.recoverer(s.logger(s.auth(mux)))
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "value", v, "path", r.URL.Path)
				fail(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path,
				"status", sw.status, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"status": "ok", "version": s.cfg.Version, "uptime": time.Since(s.started).Round(time.Second).String()})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"username": userFrom(r.Context()), "role": "admin"})
}

// ------------------------------------------------------------------ helpers

func ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func created(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": code,
		"title":  http.StatusText(code),
		"detail": msg,
	})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
