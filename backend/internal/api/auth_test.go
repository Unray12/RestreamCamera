package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/aclab/restreamcamera/internal/config"
	"github.com/aclab/restreamcamera/internal/cryptobox"
	"github.com/aclab/restreamcamera/internal/mediamtx"
	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/store"
)

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	key := bytes.Repeat([]byte{7}, 32)
	box, err := cryptobox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Version:        "test",
		AdminUser:      "admin",
		AdminPassword:  "secret",
		MasterKey:      key,
		CamerasPrefix:  "vcam-",
		SourcesPrefix:  "src-",
		MaxBulkCameras: 20,
		PublicRTSP:     "localhost:8554",
		PublicHLS:      "http://localhost:8888",
		PublicWebRTC:   "http://localhost:8889",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, st, mediamtx.New("http://127.0.0.1:1"), box, nil, log)
	return srv, st
}

func postJSON(t *testing.T, h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mediamtx/auth", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func seedCamera(t *testing.T, srv *Server, st *store.Store) (model.Source, model.Camera) {
	t.Helper()
	ctx := context.Background()
	encURL, _ := srv.box.Encrypt("rtsp://cam.lan/live")
	src, err := st.CreateSource(ctx, model.Source{Name: "cam", Kind: "pull", Enabled: true, Transport: "tcp"}, encURL)
	if err != nil {
		t.Fatal(err)
	}
	encPass, _ := srv.box.Encrypt("s3cret")
	cams := []model.Camera{{
		Name: "cam 1", Slug: "vcam-0001", SourceID: src.ID, Enabled: true,
		OnDemand: true, MaxReaders: 4, Mode: "proxy", ReadUser: "viewer", PassEnc: encPass,
	}}
	if err := st.CreateCameras(ctx, cams); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCameraBySlug(ctx, "vcam-0001")
	if err != nil {
		t.Fatal(err)
	}
	return src, got
}

func TestMediaAuthReadRules(t *testing.T) {
	srv, st := testServer(t)
	src, cam := seedCamera(t, srv, st)

	cases := []struct {
		name string
		in   mediaAuthRequest
		want int
	}{
		{
			name: "valid credentials",
			in:   mediaAuthRequest{Action: "read", Path: cam.Slug, IP: "10.0.0.9", User: "viewer", Password: "s3cret"},
			want: http.StatusOK,
		},
		{
			name: "no credentials",
			in:   mediaAuthRequest{Action: "read", Path: cam.Slug, IP: "10.0.0.9"},
			want: http.StatusUnauthorized,
		},
		{
			name: "wrong password",
			in:   mediaAuthRequest{Action: "read", Path: cam.Slug, IP: "10.0.0.9", User: "viewer", Password: "nope"},
			want: http.StatusUnauthorized,
		},
		{
			name: "unknown camera",
			in:   mediaAuthRequest{Action: "read", Path: "vcam-9999", IP: "10.0.0.9", User: "viewer", Password: "s3cret"},
			want: http.StatusUnauthorized,
		},
		{
			name: "origin path from loopback",
			in:   mediaAuthRequest{Action: "read", Path: src.Path, IP: "127.0.0.1"},
			want: http.StatusOK,
		},
		{
			name: "origin path from network",
			in:   mediaAuthRequest{Action: "read", Path: src.Path, IP: "10.0.0.9"},
			want: http.StatusForbidden,
		},
		{
			name: "publish onto a virtual camera",
			in:   mediaAuthRequest{Action: "publish", Path: cam.Slug, IP: "127.0.0.1"},
			want: http.StatusForbidden,
		},
		{
			name: "unknown action",
			in:   mediaAuthRequest{Action: "playback", Path: cam.Slug, IP: "127.0.0.1"},
			want: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, srv.handleMediaAuth, tc.in)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestMediaAuthDisabledCamera(t *testing.T) {
	srv, st := testServer(t)
	_, cam := seedCamera(t, srv, st)
	if err := st.SetCameraEnabled(context.Background(), cam.ID, false); err != nil {
		t.Fatal(err)
	}
	rec := postJSON(t, srv.handleMediaAuth, mediaAuthRequest{
		Action: "read", Path: cam.Slug, IP: "10.0.0.9", User: "viewer", Password: "s3cret",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
}

func TestMediaAuthPublisherSource(t *testing.T) {
	srv, st := testServer(t)
	src, err := st.CreateSource(context.Background(),
		model.Source{Name: "push", Kind: "publisher", Enabled: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := postJSON(t, srv.handleMediaAuth, mediaAuthRequest{
		Action: "publish", Path: src.Path, IP: "10.0.0.9",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("publish to publisher source: status %d, want 200", rec.Code)
	}
}

func TestMediaAuthIPAllowlist(t *testing.T) {
	srv, st := testServer(t)
	_, cam := seedCamera(t, srv, st)
	cam.IPAllowlist = "192.168.50.0/24"
	if err := st.UpdateCamera(context.Background(), cam); err != nil {
		t.Fatal(err)
	}

	denied := postJSON(t, srv.handleMediaAuth, mediaAuthRequest{
		Action: "read", Path: cam.Slug, IP: "10.0.0.9", User: "viewer", Password: "s3cret",
	})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("outside allowlist: status %d, want 403", denied.Code)
	}
	allowed := postJSON(t, srv.handleMediaAuth, mediaAuthRequest{
		Action: "read", Path: cam.Slug, IP: "192.168.50.4", User: "viewer", Password: "s3cret",
	})
	if allowed.Code != http.StatusOK {
		t.Fatalf("inside allowlist: status %d, want 200", allowed.Code)
	}
}

func TestMediaAuthRedirectExposesOrigin(t *testing.T) {
	srv, st := testServer(t)
	src, cam := seedCamera(t, srv, st)
	cam.Mode = "redirect"
	if err := st.UpdateCamera(context.Background(), cam); err != nil {
		t.Fatal(err)
	}
	rec := postJSON(t, srv.handleMediaAuth, mediaAuthRequest{
		Action: "read", Path: src.Path, IP: "10.0.0.9",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("redirected origin read: status %d, want 200", rec.Code)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	srv, _ := testServer(t)
	token := srv.signToken("admin", time.Minute)
	user, ok := srv.verifyToken(token)
	if !ok || user != "admin" {
		t.Fatalf("verify: %q %v", user, ok)
	}
	if _, ok := srv.verifyToken(token + "x"); ok {
		t.Fatal("tampered token accepted")
	}
	if _, ok := srv.verifyToken(srv.signToken("admin", -time.Minute)); ok {
		t.Fatal("expired token accepted")
	}
}

func TestIPHelpers(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "127.0.0.1:5000", "[::1]:5000"} {
		if !isLoopback(ip) {
			t.Errorf("%s should be loopback", ip)
		}
	}
	for _, ip := range []string{"", "10.0.0.1", "192.168.1.2:1234"} {
		if isLoopback(ip) {
			t.Errorf("%s should not be loopback", ip)
		}
	}
	cases := []struct {
		list, ip string
		want     bool
	}{
		{"", "10.0.0.1", true},
		{"10.0.0.0/8", "10.9.9.9", true},
		{"10.0.0.0/8", "11.9.9.9", false},
		{"192.168.1.10, 192.168.1.11", "192.168.1.11", true},
		{"192.168.1.10", "192.168.1.12", false},
		{"garbage", "192.168.1.12", false},
	}
	for _, tc := range cases {
		if got := ipAllowed(tc.list, tc.ip); got != tc.want {
			t.Errorf("ipAllowed(%q, %q) = %v, want %v", tc.list, tc.ip, got, tc.want)
		}
	}
}
