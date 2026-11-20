package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aclab/restreamcamera/internal/mediamtx"
)

type fakeMTX struct {
	mu       sync.Mutex
	paths    []map[string]any
	sessions []map[string]any
}

func (f *fakeMTX) set(paths, sessions []map[string]any) {
	f.mu.Lock()
	f.paths, f.sessions = paths, sessions
	f.mu.Unlock()
}

func (f *fakeMTX) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []map[string]any
		switch r.URL.Path {
		case "/v3/paths/list":
			items = f.paths
		case "/v3/rtspsessions/list":
			items = f.sessions
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"itemCount": len(items), "pageCount": 1, "items": items})
	})
}

func newTest(t *testing.T) (*Collector, *fakeMTX) {
	f := &fakeMTX{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New(mediamtx.New(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{Interval: time.Second, CameraPrefix: "vcam-", SourcePrefix: "src-", SessionEvery: 1})
	return c, f
}

func path(name string, in, out uint64, readers int) map[string]any {
	rs := make([]map[string]any, readers)
	for i := range rs {
		rs[i] = map[string]any{"type": "rtspSession", "id": name + string(rune('a'+i))}
	}
	return map[string]any{"name": name, "ready": true, "available": true, "inboundBytes": in, "outboundBytes": out, "readers": rs,
		"tracks2": []map[string]any{{"codec": "H264", "codecProps": map[string]any{"width": 1280, "height": 720}}}}
}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > want*0.01+0.01 {
		t.Fatalf("%s = %.3f, want %.3f", what, got, want)
	}
}

func TestBandwidthAndEgressFormula(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)

	// origin ingests 1 Mbps; two virtual cameras each pull it over loopback (proxy) and serve clients.
	f.set([]map[string]any{path("src-aaaa", 0, 0, 2), path("vcam-0001", 0, 0, 3), path("vcam-0002", 0, 0, 1)}, nil)
	c.Poll(ctx, t0)
	// +1s: src in 125000 B (1000 kbps), src out 250000 B (loopback to 2 cams), cam1 in 125000 B / out 375000 B, cam2 in 125000 B / out 125000 B
	f.set([]map[string]any{path("src-aaaa", 125000, 250000, 2), path("vcam-0001", 125000, 375000, 3), path("vcam-0002", 125000, 125000, 1)}, nil)
	c.Poll(ctx, t0.Add(time.Second))

	l := c.Snapshot()
	near(t, "ingest", l.InKbps, 1000)
	near(t, "egress (cams only, loopback excluded)", l.OutKbps, 3000+1000)
	near(t, "loopback", l.LoopKbps, 2000)
	if l.Readers != 4 || l.Active != 2 {
		t.Fatalf("readers=%d active=%d", l.Readers, l.Active)
	}
	p, ok := c.Path("vcam-0001")
	if !ok || p.Kind != "camera" {
		t.Fatalf("path: %+v", p)
	}
	near(t, "cam1 out", p.OutKbps, 3000)
	if len(p.Tracks) != 1 || p.Tracks[0].Codec != "H264" {
		t.Fatalf("tracks: %+v", p.Tracks)
	}
	if l.PeakOut < 3999 {
		t.Fatalf("peak %f", l.PeakOut)
	}

	// counter reset (on-demand path restarted): must not produce a negative or huge rate
	f.set([]map[string]any{path("src-aaaa", 250000, 500000, 2), path("vcam-0001", 1000, 2000, 3), path("vcam-0002", 250000, 250000, 1)}, nil)
	c.Poll(ctx, t0.Add(2*time.Second))
	p, _ = c.Path("vcam-0001")
	if p.OutKbps < 0 || p.OutKbps > 20 {
		t.Fatalf("after counter reset out=%f", p.OutKbps)
	}

	// series: global and per camera share one time grid
	gs, ps := c.Series(60, []string{"vcam-0001", "vcam-nope"})
	if len(gs.TS) != 3 || len(ps["vcam-0001"].Out) != 3 || len(ps["vcam-nope"].Out) != 3 {
		t.Fatalf("series lengths: %d %d %d", len(gs.TS), len(ps["vcam-0001"].Out), len(ps["vcam-nope"].Out))
	}
	near(t, "series cam1[1]", float64(ps["vcam-0001"].Out[1]), 3000)
	if ps["vcam-nope"].Out[1] != 0 {
		t.Fatal("unknown path must be zero filled")
	}
}

func TestIdleCameraDropsToZero(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	f.set([]map[string]any{path("vcam-0001", 0, 0, 1)}, nil)
	c.Poll(ctx, t0)
	f.set([]map[string]any{path("vcam-0001", 0, 125000, 1)}, nil)
	c.Poll(ctx, t0.Add(time.Second))
	f.set(nil, nil) // on-demand path closed
	c.Poll(ctx, t0.Add(2*time.Second))
	p, _ := c.Path("vcam-0001")
	if p.OutKbps != 0 || p.Readers != 0 || p.Ready {
		t.Fatalf("vanished path should read as idle: %+v", p)
	}
	if got := c.Snapshot(); got.OutKbps != 0 || got.Readers != 0 {
		t.Fatalf("global not zero: %+v", got)
	}
}

func TestSessionLossAndDiscards(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	sess := func(pk, lost, disc, bytes int) []map[string]any {
		return []map[string]any{{
			"id": "s1", "state": "read", "path": "vcam-0001", "remoteAddr": "10.0.0.5:50000", "userAgent": "vlc", "transport": "TCP",
			"outboundRTPPackets": pk, "outboundRTPPacketsReportedLost": lost, "outboundRTPPacketsDiscarded": disc, "outboundBytes": bytes,
		}, {
			"id": "p1", "state": "publish", "path": "src-aaaa", "remoteAddr": "127.0.0.1:1", "inboundRTPPacketsJitter": 12.5,
			"inboundRTPPackets": 1000, "inboundRTPPacketsLost": 10,
		}}
	}
	f.set([]map[string]any{path("vcam-0001", 0, 0, 1), path("src-aaaa", 0, 0, 0)}, sess(0, 0, 0, 0))
	c.Poll(ctx, t0)
	f.set([]map[string]any{path("vcam-0001", 0, 125000, 1), path("src-aaaa", 0, 0, 0)}, sess(900, 60, 40, 125000))
	c.Poll(ctx, t0.Add(time.Second))

	p, _ := c.Path("vcam-0001")
	near(t, "loss window %", p.LossPct, 10) // (60+40)/(900+60+40)
	if p.Sessions != 1 || p.Discarded != 40 || p.PacketsSent != 900 {
		t.Fatalf("session aggregate: %+v", p)
	}
	src, _ := c.Path("src-aaaa")
	near(t, "publisher jitter", src.InJitterMS, 12.5)
	near(t, "publisher loss %", src.InLossPct, 1)
	vs := c.Sessions("vcam-0001")
	if len(vs) != 1 || vs[0].Agent != "vlc" || vs[0].Internal {
		t.Fatalf("session views: %+v", vs)
	}
	near(t, "global loss", c.Snapshot().LossPct, 10)

	// next window with no new loss: window drops to 0, cumulative remembers
	f.set([]map[string]any{path("vcam-0001", 0, 250000, 1), path("src-aaaa", 0, 0, 0)}, sess(1800, 60, 40, 250000))
	c.Poll(ctx, t0.Add(2*time.Second))
	p, _ = c.Path("vcam-0001")
	if p.LossPct != 0 || p.LossTotal <= 0 {
		t.Fatalf("window=%f total=%f", p.LossPct, p.LossTotal)
	}
}

func TestMediaOffline(t *testing.T) {
	c := New(mediamtx.New("http://127.0.0.1:1"), slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	c.Poll(context.Background(), time.Now())
	if l := c.Snapshot(); l.MediaOnline || l.LastError == "" {
		t.Fatalf("expected offline with error: %+v", l)
	}
}

func TestCoarseBucket(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	var out uint64
	for i := 0; i < coarseEvery+1; i++ {
		out += 125000
		f.set([]map[string]any{path("vcam-0001", 0, out, 1)}, nil)
		c.Poll(ctx, t0.Add(time.Duration(i)*time.Second))
	}
	gs, ps := c.Series(3600, []string{"vcam-0001"})
	if gs.Step != coarseEvery || len(gs.TS) != 1 {
		t.Fatalf("coarse series: step=%d len=%d", gs.Step, len(gs.TS))
	}
	near(t, "coarse avg out", float64(ps["vcam-0001"].Out[0]), 1000*float64(coarseEvery-1)/float64(coarseEvery)+0) // first tick has no rate
}

func TestCameraSlugsComeFromTheDatabase(t *testing.T) {
	f := &fakeMTX{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	// user-chosen slugs ("lobby-1") share no prefix with anything
	c := New(mediamtx.New(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		SourcePrefix: "src-", SessionEvery: 1,
		Cameras: func(context.Context) ([]string, error) { return []string{"lobby-1"}, nil },
	})
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	f.set([]map[string]any{path("src-a", 0, 0, 1), path("lobby-1", 0, 0, 2), path("stray", 0, 0, 0)}, nil)
	c.Poll(ctx, t0)
	f.set([]map[string]any{path("src-a", 125000, 125000, 1), path("lobby-1", 125000, 250000, 2), path("stray", 0, 125000, 0)}, nil)
	c.Poll(ctx, t0.Add(time.Second))
	l := c.Snapshot()
	near(t, "egress", l.OutKbps, 2000) // lobby-1 only; "stray" is not a camera
	if l.Readers != 2 {
		t.Fatalf("readers=%d", l.Readers)
	}
	if p, _ := c.Path("lobby-1"); p.Kind != "camera" {
		t.Fatalf("kind=%s", p.Kind)
	}
	if p, _ := c.Path("stray"); p.Kind != "other" {
		t.Fatalf("kind=%s", p.Kind)
	}
}
