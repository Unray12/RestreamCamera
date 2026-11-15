// Package config loads runtime configuration from environment variables.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds every runtime knob. All values are overridable by env vars so
// the same image works in Docker Compose, systemd and local runs.
type Config struct {
	Version string

	Listen  string
	DataDir string
	DBPath  string

	MediaAPI      string
	MediaRTSP     string // RTSP base used by clients inside our network, e.g. rtsp://mediamtx:8554
	MediaRTSPLoop string // RTSP base the media server uses to reach itself, e.g. rtsp://127.0.0.1:8554
	MediaMetrics  string

	// Public base URLs returned to clients.
	PublicRTSP   string // host:port, e.g. 192.168.1.10:8554
	PublicHLS    string // http://host:8888
	PublicWebRTC string // http://host:8889

	AdminUser     string
	AdminPassword string

	MasterKey []byte

	CamerasPrefix  string
	SourcesPrefix  string
	MaxBulkCameras int

	ReconcileInterval time.Duration
	SampleInterval    time.Duration
	EventRetention    time.Duration
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// Load reads configuration and provisions the master key when missing.
func Load(version string) (*Config, error) {
	dataDir := env("RC_DATA_DIR", "./data")
	c := &Config{
		Version:        version,
		Listen:         env("RC_LISTEN", "127.0.0.1:8080"),
		DataDir:        dataDir,
		DBPath:         env("RC_DB_PATH", filepath.Join(dataDir, "restream.db")),
		MediaAPI:       strings.TrimRight(env("RC_MEDIA_API", "http://127.0.0.1:9997"), "/"),
		MediaRTSP:      strings.TrimRight(env("RC_MEDIA_RTSP_BASE", "rtsp://127.0.0.1:8554"), "/"),
		MediaRTSPLoop:  strings.TrimRight(env("RC_MEDIA_RTSP_LOOPBACK", env("RC_MEDIA_RTSP_BASE", "rtsp://127.0.0.1:8554")), "/"),
		MediaMetrics:   strings.TrimRight(env("RC_MEDIA_METRICS", "http://127.0.0.1:9998"), "/"),
		PublicRTSP:     env("RC_PUBLIC_RTSP", "localhost:8554"),
		PublicHLS:      strings.TrimRight(env("RC_PUBLIC_HLS", "http://localhost:8888"), "/"),
		PublicWebRTC:   strings.TrimRight(env("RC_PUBLIC_WEBRTC", "http://localhost:8889"), "/"),
		AdminUser:      env("RC_ADMIN_USER", "admin"),
		AdminPassword:  env("RC_ADMIN_PASSWORD", "admin"),
		CamerasPrefix:  env("RC_CAMERA_PREFIX", "vcam-"),
		SourcesPrefix:  env("RC_SOURCE_PREFIX", "src-"),
		MaxBulkCameras: envInt("RC_MAX_BULK_CAMERAS", 500),

		ReconcileInterval: envDuration("RC_RECONCILE_INTERVAL", 5*time.Second),
		SampleInterval:    envDuration("RC_SAMPLE_INTERVAL", time.Minute),
		EventRetention:    envDuration("RC_EVENT_RETENTION", 7*24*time.Hour),
	}

	key, err := loadMasterKey(c)
	if err != nil {
		return nil, err
	}
	c.MasterKey = key
	return c, nil
}

// loadMasterKey resolves RC_MASTER_KEY (base64), RC_MASTER_KEY_FILE or a
// generated key persisted in the data directory.
func loadMasterKey(c *Config) ([]byte, error) {
	if raw := strings.TrimSpace(os.Getenv("RC_MASTER_KEY")); raw != "" {
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("RC_MASTER_KEY must be base64: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("RC_MASTER_KEY must decode to 32 bytes, got %d", len(key))
		}
		return key, nil
	}
	if path := strings.TrimSpace(os.Getenv("RC_MASTER_KEY_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read RC_MASTER_KEY_FILE: %w", err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("RC_MASTER_KEY_FILE must contain base64: %w", err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
		}
		return key, nil
	}
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(c.DataDir, "master.key")
	if raw, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("invalid master key at %s", path)
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
