// Package provision keeps MediaMTX paths in sync with the database.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aclab/restreamcamera/internal/config"
	"github.com/aclab/restreamcamera/internal/cryptobox"
	"github.com/aclab/restreamcamera/internal/mediamtx"
	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/rtsp"
	"github.com/aclab/restreamcamera/internal/store"
)

// Stats is a snapshot of the reconciler activity.
type Stats struct {
	Readers       int       `json:"readers"`
	InKbps        int64     `json:"inKbps"`
	OutKbps       int64     `json:"outKbps"`
	ManagedPaths  int       `json:"managedPaths"`
	LastRunAt     time.Time `json:"lastRunAt"`
	LastError     string    `json:"lastError"`
	Adds          int       `json:"adds"`
	Patches       int       `json:"patches"`
	Deletes       int       `json:"deletes"`
	TotalInBytes  uint64    `json:"totalInBytes"`
	TotalOutBytes uint64    `json:"totalOutBytes"`
}

type byteSample struct {
	in, out uint64
	at      time.Time
}

// Reconciler owns the desired-state loop.
type Reconciler struct {
	st  *store.Store
	mtx *mediamtx.Client
	box *cryptobox.Box
	cfg *config.Config
	log *slog.Logger

	trigger chan struct{}

	mu        sync.RWMutex
	stats     Stats
	lastSeen  map[string]byteSample
	lastRun   time.Time
	probing   bool
	lastProbe time.Time
}

// New creates a reconciler.
func New(st *store.Store, mtx *mediamtx.Client, box *cryptobox.Box, cfg *config.Config, log *slog.Logger) *Reconciler {
	return &Reconciler{
		st:       st,
		mtx:      mtx,
		box:      box,
		cfg:      cfg,
		log:      log,
		trigger:  make(chan struct{}, 1),
		lastSeen: map[string]byteSample{},
	}
}

// Trigger requests an immediate reconciliation.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Stats returns the latest stats snapshot.
func (r *Reconciler) Stats() Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stats
}

// Run loops until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	tick := time.NewTicker(r.cfg.ReconcileInterval)
	defer tick.Stop()
	sample := time.NewTicker(r.cfg.SampleInterval)
	defer sample.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()

	r.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.runOnce(ctx)
		case <-r.trigger:
			r.runOnce(ctx)
		case <-sample.C:
			r.writeSample(ctx)
		case <-prune.C:
			_ = r.st.PruneEvents(ctx, r.cfg.EventRetention)
			_ = r.st.PruneSamples(ctx, time.Now().Add(-7*24*time.Hour).Unix())
		}
	}
}

func (r *Reconciler) runOnce(ctx context.Context) {
	start := time.Now()
	err := r.Reconcile(ctx)
	r.mu.Lock()
	r.stats.LastRunAt = time.Now().UTC()
	if err != nil {
		r.stats.LastError = err.Error()
	} else {
		r.stats.LastError = ""
	}
	r.mu.Unlock()
	if err == nil {
		r.log.Debug("reconcile ok", "took", time.Since(start).Round(time.Millisecond))
	}
}

// Reconcile performs one desired/actual comparison.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	sources, err := r.st.ListSources(ctx)
	if err != nil {
		return fmt.Errorf("list sources: %w", err)
	}
	cameras, err := r.st.AllCamerasForReconcile(ctx)
	if err != nil {
		return fmt.Errorf("list cameras: %w", err)
	}

	srcByID := make(map[string]model.Source, len(sources))
	desired := make(map[string]mediamtx.PathConf, len(cameras)+len(sources))
	srcPath := make(map[string]string, len(sources)) // source id -> path name

	for _, s := range sources {
		srcByID[s.ID] = s
		if !s.Enabled {
			continue
		}
		name := r.cfg.SourcesPrefix + shortID(s.ID)
		srcPath[s.ID] = name
		conf := mediamtx.PathConf{}
		if s.Kind == "publisher" {
			// A publisher path must not request on-demand: clients push into it.
			conf.Source = "publisher"
		} else {
			raw, err := r.box.Decrypt(s.URLEnc)
			if err != nil {
				r.log.Warn("cannot decrypt source url", "source", s.Name, "err", err)
				continue
			}
			conf.Source = raw
			conf.SourceOnDemand = true
			conf.RTSPTransport = s.Transport
		}
		desired[name] = conf
	}

	cameraPath := make(map[string]model.Camera, len(cameras))
	for _, c := range cameras {
		if !c.Enabled {
			r.setRuntime(ctx, c, model.Runtime{State: "disabled"})
			continue
		}
		sp, ok := srcPath[c.SourceID]
		if !ok {
			r.setRuntime(ctx, c, model.Runtime{State: "error", LastError: "source unavailable"})
			continue
		}
		conf := mediamtx.PathConf{
			SourceOnDemand: c.OnDemand,
			MaxReaders:     c.MaxReaders,
		}
		if c.Mode == "redirect" {
			conf.Source = "redirect"
			conf.SourceRedirect = "rtsp://" + r.cfg.PublicRTSP + "/" + sp
		} else {
			conf.Source = r.cfg.MediaRTSPLoop + "/" + sp
		}
		desired[c.Slug] = conf
		cameraPath[c.Slug] = c
	}

	actualList, err := r.mtx.ListConfigPaths(ctx)
	if err != nil {
		return fmt.Errorf("list mediamtx paths: %w", err)
	}
	actual := make(map[string]mediamtx.PathConf, len(actualList))
	for _, p := range actualList {
		actual[p.Name] = p
	}

	var (
		toAdd    []string
		toPatch  []string
		toDelete []string
	)
	for name, want := range desired {
		cur, ok := actual[name]
		if !ok {
			toAdd = append(toAdd, name)
			continue
		}
		if pathDiffers(cur, want) {
			toPatch = append(toPatch, name)
		}
	}
	for name := range actual {
		if _, ok := desired[name]; ok {
			continue
		}
		if strings.HasPrefix(name, r.cfg.CamerasPrefix) || strings.HasPrefix(name, r.cfg.SourcesPrefix) {
			toDelete = append(toDelete, name)
		}
	}

	var (
		adds, patches, deletes int
		firstErr               error
	)
	run := func(names []string, fn func(string) error) int {
		const workers = 8
		var (
			wg sync.WaitGroup
			mu sync.Mutex
			n  int
		)
		ch := make(chan string)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for name := range ch {
					if err := fn(name); err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						r.log.Warn("path operation failed", "path", name, "err", err)
						continue
					}
					mu.Lock()
					n++
					mu.Unlock()
				}
			}()
		}
		for _, name := range names {
			ch <- name
		}
		close(ch)
		wg.Wait()
		return n
	}

	adds = run(toAdd, func(name string) error {
		return r.mtx.AddPath(ctx, name, desired[name])
	})
	patches = run(toPatch, func(name string) error {
		return r.mtx.PatchPath(ctx, name, desired[name])
	})
	deletes = run(toDelete, func(name string) error {
		return r.mtx.DeletePath(ctx, name)
	})
	if adds+patches+deletes > 0 {
		r.log.Info("reconciled media paths",
			"added", adds, "patched", patches, "deleted", deletes, "desired", len(desired))
		r.logEvent(ctx, "info", "reconcile",
			fmt.Sprintf("added %d, patched %d, deleted %d paths", adds, patches, deletes))
	}

	// runtime state
	states, err := r.mtx.ListPaths(ctx)
	if err != nil {
		return fmt.Errorf("list mediamtx runtime paths: %w", err)
	}
	stateByName := make(map[string]mediamtx.PathState, len(states))
	for _, s := range states {
		stateByName[s.Name] = s
	}
	now := time.Now()
	var (
		readers  int
		inKbps   int64
		outKbps  int64
		totalIn  uint64
		totalOut uint64
	)
	for name, c := range cameraPath {
		st, ok := stateByName[name]
		if !ok {
			state := "unknown"
			if c.OnDemand {
				// Configured but not instantiated until a client connects.
				state = "idle"
			}
			r.setRuntime(ctx, c, model.Runtime{State: state})
			continue
		}
		prev := r.lastSeen[name]
		var inRate, outRate int64
		if !prev.at.IsZero() {
			dt := now.Sub(prev.at).Seconds()
			if dt > 0.5 {
				if st.InboundBytes >= prev.in {
					inRate = int64(float64(st.InboundBytes-prev.in) * 8 / 1000 / dt)
				}
				if st.OutboundBytes >= prev.out {
					outRate = int64(float64(st.OutboundBytes-prev.out) * 8 / 1000 / dt)
				}
			}
		}
		r.lastSeen[name] = byteSample{in: st.InboundBytes, out: st.OutboundBytes, at: now}

		camReaders := len(st.Readers)
		state := "idle"
		switch {
		case st.Ready || st.Available:
			state = "ready"
		case camReaders > 0:
			// A client is connected but no media is flowing.
			state = "offline"
		}
		rt := model.Runtime{
			State:         state,
			Readers:       camReaders,
			InKbps:        inRate,
			OutKbps:       outRate,
			InboundBytes:  st.InboundBytes,
			OutboundBytes: st.OutboundBytes,
		}
		r.setRuntime(ctx, c, rt)
		readers += camReaders
		inKbps += inRate
		outKbps += outRate
		totalIn += st.InboundBytes
		totalOut += st.OutboundBytes
	}

	// source status, cheap check based on the origin path when it exists
	for _, s := range sources {
		name := r.cfg.SourcesPrefix + shortID(s.ID)
		st, ok := stateByName[name]
		status := "idle"
		lastErr := ""
		if ok {
			if st.Ready || st.Available {
				status = "online"
			} else {
				status = "offline"
				lastErr = "origin path not ready"
			}
		}
		if !s.Enabled {
			status = "disabled"
		}
		if status != s.Status || lastErr != s.LastError {
			_ = r.st.UpdateSourceStatus(ctx, s.ID, status, lastErr)
		}
	}

	r.mu.Lock()
	r.stats.Readers = readers
	r.stats.InKbps = inKbps
	r.stats.OutKbps = outKbps
	r.stats.ManagedPaths = len(desired)
	r.stats.TotalInBytes = totalIn
	r.stats.TotalOutBytes = totalOut
	r.stats.Adds, r.stats.Patches, r.stats.Deletes = adds, patches, deletes
	r.mu.Unlock()

	// periodic probe of pull sources (does not pull media)
	if time.Since(r.lastProbe) > time.Minute {
		r.lastProbe = now
		go r.probeSources(sources)
	}
	return firstErr
}

func (r *Reconciler) probeSources(sources []model.Source) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, s := range sources {
		if !s.Enabled || s.Kind != "pull" {
			continue
		}
		raw, err := r.box.Decrypt(s.URLEnc)
		if err != nil {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		switch u.Scheme {
		case "rtsp", "rtsps":
			res := rtsp.Describe(ctx, raw, 5*time.Second)
			status, lastErr := "online", ""
			if !res.OK {
				status = "offline"
				lastErr = res.Error
			}
			_ = r.st.UpdateSourceStatus(ctx, s.ID, status, lastErr)
		}
	}
}

func (r *Reconciler) setRuntime(ctx context.Context, c model.Camera, rt model.Runtime) {
	// skip writes when nothing changed to keep SQLite quiet
	if c.Runtime.State == rt.State && c.Runtime.Readers == rt.Readers &&
		c.Runtime.InboundBytes == rt.InboundBytes && c.Runtime.OutboundBytes == rt.OutboundBytes &&
		c.Runtime.LastError == rt.LastError {
		return
	}
	rt.UpdatedAt = time.Now().UTC()
	if err := r.st.SetRuntime(ctx, c.ID, rt); err != nil {
		r.log.Warn("set runtime", "camera", c.Slug, "err", err)
	}
}

func (r *Reconciler) writeSample(ctx context.Context) {
	st := r.Stats()
	overview, err := r.st.Overview(ctx)
	if err != nil {
		return
	}
	_ = r.st.AddSample(ctx, time.Now().Unix(), st.Readers, st.TotalInBytes, st.TotalOutBytes, overview.CamerasReady)
}

func (r *Reconciler) logEvent(ctx context.Context, level, category, message string) {
	_ = r.st.AddEvent(ctx, level, category, "", message)
}

func pathDiffers(a, b mediamtx.PathConf) bool {
	if a.Source != b.Source ||
		a.SourceOnDemand != b.SourceOnDemand ||
		a.SourceRedirect != b.SourceRedirect ||
		a.MaxReaders != b.MaxReaders {
		return true
	}
	// The server reports its default as "automatic"; treat it as unset.
	at, bt := a.RTSPTransport, b.RTSPTransport
	if at == "automatic" {
		at = ""
	}
	if bt == "automatic" {
		bt = ""
	}
	return at != bt
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ErrNoMediaServer indicates MediaMTX is unreachable.
var ErrNoMediaServer = errors.New("media server unreachable")
