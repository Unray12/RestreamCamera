// Command restreamd is the RestreamCamera control plane: REST API, embedded UI,
// SQLite state and the MediaMTX reconciler.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aclab/restreamcamera/internal/api"
	"github.com/aclab/restreamcamera/internal/config"
	"github.com/aclab/restreamcamera/internal/cryptobox"
	"github.com/aclab/restreamcamera/internal/mediamtx"
	"github.com/aclab/restreamcamera/internal/provision"
	"github.com/aclab/restreamcamera/internal/store"
	"github.com/aclab/restreamcamera/internal/telemetry"
)

// version is injected at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "backup":
			os.Exit(backup(os.Args[2:]))
		case "version", "--version", "-v":
			fmt.Println(version)
			return
		case "serve":
			// fallthrough
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q (serve|healthcheck|backup|version)\n", os.Args[1])
			os.Exit(2)
		}
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func logger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("RC_LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("app", "restreamcamera", "version", version)
}

func serve() error {
	log := logger()
	cfg, err := config.Load(version)
	if err != nil {
		return err
	}
	if cfg.AdminPassword == "admin" {
		log.Warn("using default admin password, set RC_ADMIN_PASSWORD before exposing this instance")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	box, err := cryptobox.New(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("crypto: %w", err)
	}
	mtx := mediamtx.New(cfg.MediaAPI)
	rec := provision.New(st, mtx, box, cfg, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go rec.Run(ctx)

	tel := telemetry.New(mtx, log, telemetry.Options{
		Interval:     cfg.TelemetryInterval,
		CameraPrefix: cfg.CamerasPrefix,
		SourcePrefix: cfg.SourcesPrefix,
		RTSPAddr:     telemetry.RTSPHostPort(cfg.MediaRTSP),
		Cameras: func(ctx context.Context) ([]string, error) {
			cams, err := st.AllCamerasForReconcile(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, len(cams))
			for i, c := range cams {
				out[i] = c.Slug
			}
			return out, nil
		},
	})
	go tel.Run(ctx)

	apiSrv := api.New(cfg, st, mtx, box, rec, log)
	apiSrv.SetTelemetry(tel)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "mediaApi", cfg.MediaAPI,
			"dataDir", cfg.DataDir, "publicRtsp", cfg.PublicRTSP)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// healthcheck probes the local API, used by Docker HEALTHCHECK.
func healthcheck() int {
	addr := os.Getenv("RC_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = "127.0.0.1", "8080"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", res.StatusCode)
		return 1
	}
	return 0
}

// backup copies the SQLite database to the given path.
func backup(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: restreamd backup <target.db>")
		return 2
	}
	cfg, err := config.Load(version)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	target := args[0]
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := st.BackupTo(context.Background(), target); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("backup written to", target)
	return 0
}
