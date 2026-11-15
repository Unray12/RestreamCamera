package provision

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aclab/restreamcamera/internal/config"
	"github.com/aclab/restreamcamera/internal/cryptobox"
	"github.com/aclab/restreamcamera/internal/mediamtx"
	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/store"
)

// fakeMTX is an in-memory MediaMTX Control API.
type fakeMTX struct {
	mu      sync.Mutex
	paths   map[string]mediamtx.PathConf
	ready   map[string]bool
	readers map[string]int
	adds    int
	patches int
	deletes int
}

func newFakeMTX() *fakeMTX {
	return &fakeMTX{
		paths:   map[string]mediamtx.PathConf{},
		ready:   map[string]bool{},
		readers: map[string]int{},
	}
}

func (f *fakeMTX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	writeItems := func(items any, count int) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"itemCount": count, "pageCount": 1, "items": items,
		})
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v3/config/paths/list":
		items := make([]mediamtx.PathConf, 0, len(f.paths))
		for _, p := range f.paths {
			items = append(items, p)
		}
		writeItems(items, len(items))

	case r.Method == http.MethodGet && r.URL.Path == "/v3/paths/list":
		items := make([]mediamtx.PathState, 0, len(f.paths))
		for name := range f.paths {
			readers := make([]mediamtx.Reader, f.readers[name])
			for i := range readers {
				readers[i] = mediamtx.Reader{Type: "rtspSession"}
			}
			items = append(items, mediamtx.PathState{
				Name: name, Ready: f.ready[name], Readers: readers,
				InboundBytes: 1 << 20, OutboundBytes: 2 << 20,
			})
		}
		writeItems(items, len(items))

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v3/config/paths/add/"):
		name := strings.TrimPrefix(r.URL.Path, "/v3/config/paths/add/")
		var conf mediamtx.PathConf
		_ = json.NewDecoder(r.Body).Decode(&conf)
		conf.Name = name
		f.paths[name] = conf
		f.adds++

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v3/config/paths/patch/"):
		name := strings.TrimPrefix(r.URL.Path, "/v3/config/paths/patch/")
		var conf mediamtx.PathConf
		_ = json.NewDecoder(r.Body).Decode(&conf)
		conf.Name = name
		f.paths[name] = conf
		f.patches++

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v3/config/paths/delete/"):
		name := strings.TrimPrefix(r.URL.Path, "/v3/config/paths/delete/")
		delete(f.paths, name)
		f.deletes++

	default:
		http.NotFound(w, r)
	}
}

type fixture struct {
	rec  *Reconciler
	st   *store.Store
	fake *fakeMTX
	box  *cryptobox.Box
	src  model.Source
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key := make([]byte, 32)
	box, _ := cryptobox.New(key)
	cfg := &config.Config{
		CamerasPrefix:     "vcam-",
		SourcesPrefix:     "src-",
		MediaRTSP:         "rtsp://mediamtx:8554",
		MediaRTSPLoop:     "rtsp://127.0.0.1:8554",
		PublicRTSP:        "cams.lan:8554",
		ReconcileInterval: 5 * time.Second,
	}
	fake := newFakeMTX()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := New(st, mediamtx.New(srv.URL), box, cfg, log)
	rec.lastProbe = time.Now() // do not spawn network probes in tests

	enc, _ := box.Encrypt("rtsp://camera.lan/live")
	src, err := st.CreateSource(context.Background(),
		model.Source{Name: "cam", Kind: "pull", Enabled: true, Transport: "tcp"}, enc)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{rec: rec, st: st, fake: fake, box: box, src: src}
}

func (f *fixture) addCameras(t *testing.T, n int) []model.Camera {
	t.Helper()
	cams := make([]model.Camera, 0, n)
	for i := 1; i <= n; i++ {
		cams = append(cams, model.Camera{
			Name: "cam", Slug: "vcam-000" + string(rune('0'+i)),
			SourceID: f.src.ID, Enabled: true, OnDemand: true,
			MaxReaders: 4, Mode: "proxy", ReadUser: "u",
		})
	}
	if err := f.st.CreateCameras(context.Background(), cams); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.AllCamerasForReconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReconcileCreatesPaths(t *testing.T) {
	f := newFixture(t)
	f.addCameras(t, 3)
	ctx := context.Background()

	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if len(f.fake.paths) != 4 {
		t.Fatalf("got %d paths, want 4 (1 origin + 3 cameras)", len(f.fake.paths))
	}
	origin, ok := f.fake.paths[f.src.Path]
	if !ok {
		t.Fatalf("origin path %s missing", f.src.Path)
	}
	if origin.Source != "rtsp://camera.lan/live" {
		t.Fatalf("origin source = %q", origin.Source)
	}
	cam, ok := f.fake.paths["vcam-0001"]
	if !ok {
		t.Fatal("vcam-0001 missing")
	}
	want := "rtsp://127.0.0.1:8554/" + f.src.Path
	if cam.Source != want {
		t.Fatalf("camera source = %q, want %q", cam.Source, want)
	}
	if !cam.SourceOnDemand || cam.MaxReaders != 4 {
		t.Fatalf("camera conf = %+v", cam)
	}

	// Runtime state is persisted from /v3/paths/list.
	got, err := f.st.GetCameraBySlug(ctx, "vcam-0001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime.State != "idle" {
		t.Fatalf("runtime state = %q, want idle", got.Runtime.State)
	}
}

func TestReconcileAggregatesReaders(t *testing.T) {
	f := newFixture(t)
	f.addCameras(t, 2)
	f.fake.mu.Lock()
	f.fake.ready["vcam-0001"] = true
	f.fake.readers["vcam-0001"] = 3
	f.fake.mu.Unlock()

	if err := f.rec.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.rec.Stats().Readers; got != 3 {
		t.Fatalf("stats readers = %d, want 3", got)
	}
	cam, err := f.st.GetCameraBySlug(context.Background(), "vcam-0001")
	if err != nil {
		t.Fatal(err)
	}
	if cam.Runtime.State != "ready" || cam.Runtime.Readers != 3 {
		t.Fatalf("runtime = %+v, want ready/3", cam.Runtime)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.addCameras(t, 2)
	ctx := context.Background()
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stats := f.rec.Stats()
	if stats.Adds != 3 {
		t.Fatalf("first pass adds = %d, want 3", stats.Adds)
	}
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	stats = f.rec.Stats()
	if stats.Adds != 0 || stats.Patches != 0 || stats.Deletes != 0 {
		t.Fatalf("second pass was not idempotent: %+v", stats)
	}
}

func TestReconcilePatchesChangedCamera(t *testing.T) {
	f := newFixture(t)
	cams := f.addCameras(t, 1)
	ctx := context.Background()
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	cam := cams[0]
	cam.MaxReaders = 12
	if err := f.st.UpdateCamera(ctx, cam); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if f.fake.patches != 1 {
		t.Fatalf("patches = %d, want 1", f.fake.patches)
	}
	if got := f.fake.paths[cam.Slug].MaxReaders; got != 12 {
		t.Fatalf("maxReaders = %d, want 12", got)
	}
}

func TestReconcileDeletesDisabledAndOrphans(t *testing.T) {
	f := newFixture(t)
	cams := f.addCameras(t, 2)
	ctx := context.Background()
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	// an orphan managed path and an unrelated path that must survive
	f.fake.mu.Lock()
	f.fake.paths["vcam-orphan"] = mediamtx.PathConf{Name: "vcam-orphan"}
	f.fake.paths["someone-else"] = mediamtx.PathConf{Name: "someone-else"}
	f.fake.mu.Unlock()

	if err := f.st.SetCameraEnabled(ctx, cams[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if _, ok := f.fake.paths[cams[0].Slug]; ok {
		t.Fatalf("%s still configured after disable", cams[0].Slug)
	}
	if _, ok := f.fake.paths["vcam-orphan"]; ok {
		t.Fatal("orphan vcam path was not deleted")
	}
	if _, ok := f.fake.paths["someone-else"]; !ok {
		t.Fatal("unmanaged path was deleted")
	}
}

func TestReconcileIgnoresServerDefaults(t *testing.T) {
	f := newFixture(t)
	f.addCameras(t, 1)
	ctx := context.Background()
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// MediaMTX reports unset rtspTransport as "automatic"; that must not look
	// like a configuration drift.
	f.fake.mu.Lock()
	conf := f.fake.paths["vcam-0001"]
	conf.RTSPTransport = "automatic"
	f.fake.paths["vcam-0001"] = conf
	f.fake.mu.Unlock()

	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := f.rec.Stats(); stats.Patches != 0 {
		t.Fatalf("patches = %d, want 0", stats.Patches)
	}
}

func TestPathDiffers(t *testing.T) {
	base := mediamtx.PathConf{Source: "rtsp://x/y", SourceOnDemand: true, MaxReaders: 4}
	if pathDiffers(base, base) {
		t.Fatal("identical configs should match")
	}
	auto := base
	auto.RTSPTransport = "automatic"
	if pathDiffers(auto, base) {
		t.Fatal("automatic transport should equal unset")
	}
	tcp := base
	tcp.RTSPTransport = "tcp"
	if !pathDiffers(tcp, base) {
		t.Fatal("tcp transport should differ from unset")
	}
	more := base
	more.MaxReaders = 20
	if !pathDiffers(more, base) {
		t.Fatal("maxReaders change was not detected")
	}
	redirect := base
	redirect.Source = "redirect"
	redirect.SourceRedirect = "rtsp://a/b"
	if !pathDiffers(redirect, base) {
		t.Fatal("redirect change was not detected")
	}
}

func TestReconcilePublisherAndRedirect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pub, err := f.st.CreateSource(ctx, model.Source{Name: "push", Kind: "publisher", Enabled: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateCameras(ctx, []model.Camera{
		{Name: "r", Slug: "vcam-redirect", SourceID: f.src.ID, Enabled: true,
			MaxReaders: 2, Mode: "redirect"},
		{Name: "p", Slug: "vcam-push", SourceID: pub.ID, Enabled: true,
			MaxReaders: 2, Mode: "proxy"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if got := f.fake.paths[pub.Path].Source; got != "publisher" {
		t.Fatalf("publisher source = %q", got)
	}
	if f.fake.paths[pub.Path].SourceOnDemand {
		t.Fatal("publisher path must not be on-demand")
	}
	red, ok := f.fake.paths["vcam-redirect"]
	if !ok {
		t.Fatal("redirect camera missing")
	}
	if red.Source != "redirect" {
		t.Fatalf("redirect source = %q", red.Source)
	}
	want := "rtsp://cams.lan:8554/" + f.src.Path
	if red.SourceRedirect != want {
		t.Fatalf("sourceRedirect = %q, want %q", red.SourceRedirect, want)
	}
}
