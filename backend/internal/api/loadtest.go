package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/rtsp"
)

const maxRuns = 20

// Dist is a distribution summary of one metric over the clients of a run.
type Dist struct {
	N   int     `json:"n"`
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

func dist(vals []float64) Dist {
	if len(vals) == 0 {
		return Dist{}
	}
	v := append([]float64(nil), vals...)
	sort.Float64s(v)
	var sum float64
	for _, x := range v {
		sum += x
	}
	pct := func(p float64) float64 { return v[int(float64(len(v)-1)*p)] }
	return Dist{N: len(v), Min: v[0], Avg: sum / float64(len(v)), P50: pct(0.5), P95: pct(0.95), P99: pct(0.99), Max: v[len(v)-1]}
}

// loadPoint is one second of a run.
type loadPoint struct {
	T       int     `json:"t"` // seconds since start
	Active  int     `json:"active"`
	Mbps    float64 `json:"mbps"`
	FPS     float64 `json:"fps"`     // total frames per second over every client
	AvgKbps float64 `json:"avgKbps"` // mean per playing client
	P5Kbps  float64 `json:"p5Kbps"`  // the slowest 5 % of clients
	Lost    uint64  `json:"lost"`    // packets lost in this second
	Fail    int     `json:"fail"`    // cumulative failed clients
	Stalls  uint64  `json:"stalls"`  // stalls in this second
}

type camStat struct {
	Slug    string  `json:"slug"`
	Clients int     `json:"clients"`
	OK      int     `json:"ok"`
	Fail    int     `json:"fail"`
	AvgKbps float64 `json:"avgKbps"`
	AvgTTFB float64 `json:"avgTtfbMs"`
	Loss    float64 `json:"lossPct"`
}

type clientResult struct {
	slug string
	ok   bool
	err  string
	res  rtsp.PlayResult
	desc rtsp.Result
}

type loadRun struct {
	ID         string     `json:"id"`
	Mode       string     `json:"mode"`   // describe | stream
	Status     string     `json:"status"` // running | done
	Stopped    bool       `json:"stopped"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	Count       int `json:"count"`
	Concurrency int `json:"concurrency"`
	TimeoutMS   int `json:"timeoutMs"`
	DurationSec int `json:"durationSec"`
	RampSec     int `json:"rampSec"`
	Cameras     int `json:"cameras"` // distinct cameras targeted

	// progress
	Started int `json:"started"`
	Active  int `json:"active"` // clients currently receiving media (stream) / in flight (describe)
	Done    int `json:"done"`
	OK      int `json:"ok"`
	Fail    int `json:"fail"`
	Dropped int `json:"dropped"` // clients that lost the connection mid-stream

	// legacy DESCRIBE latency summary (ms)
	AvgMS int64 `json:"avgMs"`
	P50MS int64 `json:"p50Ms"`
	P95MS int64 `json:"p95Ms"`
	MaxMS int64 `json:"maxMs"`

	// distributions
	Handshake Dist `json:"handshake"` // dial -> PLAY accepted (stream) / DESCRIBE (describe)
	Describe  Dist `json:"describe"`
	TTFB      Dist `json:"ttfb"`    // PLAY -> first RTP packet
	Start     Dist `json:"start"`   // dial -> first RTP packet
	Bitrate   Dist `json:"bitrate"` // kbps per client over its whole session
	FPS       Dist `json:"fps"`
	Jitter    Dist `json:"jitter"` // ms
	MaxGap    Dist `json:"maxGap"` // ms, longest silence per client
	Delay     Dist `json:"delay"`  // ms, server relay delay measured from RTCP SR

	// totals
	TotalBytes     uint64  `json:"totalBytes"`
	TotalPackets   uint64  `json:"totalPackets"`
	TotalLost      uint64  `json:"totalLost"`
	TotalFrames    uint64  `json:"totalFrames"`
	LossPct        float64 `json:"lossPct"`
	Stalls         uint64  `json:"stalls"`
	StalledClients int     `json:"stalledClients"`
	AvgMbps        float64 `json:"avgMbps"`
	PeakMbps       float64 `json:"peakMbps"`

	Errors     map[string]int `json:"errors"`
	Samples    []string       `json:"samples"`
	DurationMS int64          `json:"durationMs"`
	ElapsedMS  int64          `json:"elapsedMs"`

	Series    []loadPoint `json:"series,omitempty"`
	PerCamera []camStat   `json:"perCamera,omitempty"`

	cancel context.CancelFunc
}

type runStore struct {
	mu    sync.Mutex
	runs  map[string]*loadRun
	order []string
}

func newRunStore() *runStore { return &runStore{runs: map[string]*loadRun{}} }

func (rs *runStore) put(r *loadRun) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.runs[r.ID] = r
	rs.order = append(rs.order, r.ID)
	for len(rs.order) > maxRuns {
		delete(rs.runs, rs.order[0])
		rs.order = rs.order[1:]
	}
}

func cloneRun(r *loadRun, light bool) loadRun {
	cp := *r
	cp.cancel = nil
	cp.Errors = make(map[string]int, len(r.Errors))
	for k, v := range r.Errors {
		cp.Errors[k] = v
	}
	cp.Samples = append([]string(nil), r.Samples...)
	if light {
		cp.Series, cp.PerCamera = nil, nil
	} else {
		cp.Series = append([]loadPoint(nil), r.Series...)
		cp.PerCamera = append([]camStat(nil), r.PerCamera...)
	}
	return cp
}

func (rs *runStore) get(id string) (loadRun, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.runs[id]
	if !ok {
		return loadRun{}, false
	}
	return cloneRun(r, false), true
}

func (rs *runStore) list() []loadRun {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]loadRun, 0, len(rs.order))
	for i := len(rs.order) - 1; i >= 0; i-- {
		out = append(out, cloneRun(rs.runs[rs.order[i]], true))
	}
	return out
}

func (rs *runStore) running() *loadRun {
	for _, id := range rs.order {
		if r := rs.runs[id]; r.Status == "running" {
			return r
		}
	}
	return nil
}

// LoadLive is the compact view of the active run pushed on the dashboard stream.
type LoadLive struct {
	ID        string  `json:"id"`
	Mode      string  `json:"mode"`
	Status    string  `json:"status"`
	Count     int     `json:"count"`
	Active    int     `json:"active"`
	Done      int     `json:"done"`
	OK        int     `json:"ok"`
	Fail      int     `json:"fail"`
	Mbps      float64 `json:"mbps"`
	FPS       float64 `json:"fps"`
	AvgKbps   float64 `json:"avgKbps"`
	P5Kbps    float64 `json:"p5Kbps"`
	TTFBP95   float64 `json:"ttfbP95"`
	LossPct   float64 `json:"lossPct"`
	ElapsedMS int64   `json:"elapsedMs"`
	DurationS int     `json:"durationSec"`
	CreatedAt int64   `json:"createdAt"`
}

// loadLive returns the running run, or the most recent finished one within 2 minutes.
func (s *Server) loadLive() *LoadLive {
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	var r *loadRun
	if r = s.runs.running(); r == nil && len(s.runs.order) > 0 {
		last := s.runs.runs[s.runs.order[len(s.runs.order)-1]]
		if last.FinishedAt != nil && time.Since(*last.FinishedAt) < 2*time.Minute {
			r = last
		}
	}
	if r == nil {
		return nil
	}
	l := &LoadLive{ID: r.ID, Mode: r.Mode, Status: r.Status, Count: r.Count, Active: r.Active, Done: r.Done,
		OK: r.OK, Fail: r.Fail, TTFBP95: r.TTFB.P95, LossPct: r.LossPct, ElapsedMS: r.ElapsedMS,
		DurationS: r.DurationSec, CreatedAt: r.CreatedAt.Unix()}
	if n := len(r.Series); n > 0 {
		p := r.Series[n-1]
		l.Mbps, l.FPS, l.AvgKbps, l.P5Kbps = p.Mbps, p.FPS, p.AvgKbps, p.P5Kbps
	}
	return l
}

func (s *Server) handleStartLoadtest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode        string   `json:"mode"`
		Count       int      `json:"count"`
		Concurrency int      `json:"concurrency"`
		TimeoutMS   int      `json:"timeoutMs"`
		DurationSec int      `json:"durationSec"`
		RampSec     int      `json:"rampSec"`
		CameraIDs   []string `json:"cameraIds"`
		SourceID    string   `json:"sourceId"`
		Group       string   `json:"group"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Mode == "" {
		in.Mode = "describe"
	}
	if in.Mode != "describe" && in.Mode != "stream" {
		fail(w, http.StatusBadRequest, "mode must be describe or stream")
		return
	}
	if in.Count <= 0 || in.Count > 2000 {
		fail(w, http.StatusBadRequest, "count must be between 1 and 2000")
		return
	}
	if in.Concurrency <= 0 {
		in.Concurrency = 16
	}
	if in.Concurrency > 200 {
		in.Concurrency = 200
	}
	if in.TimeoutMS <= 0 {
		in.TimeoutMS = 8000
	}
	if in.Mode == "stream" {
		if in.DurationSec <= 0 {
			in.DurationSec = 30
		}
		if in.DurationSec < 3 || in.DurationSec > 3600 {
			fail(w, http.StatusBadRequest, "durationSec must be between 3 and 3600")
			return
		}
		if in.RampSec < 0 || in.RampSec > 600 {
			fail(w, http.StatusBadRequest, "rampSec must be between 0 and 600")
			return
		}
	} else {
		in.DurationSec, in.RampSec = 0, 0
	}

	cams, err := s.allCameras(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := map[string]bool{}
	for _, id := range in.CameraIDs {
		want[id] = true
	}
	selected := make([]model.Camera, 0, len(cams))
	for _, c := range cams {
		if !c.Enabled {
			continue
		}
		if in.SourceID != "" && c.SourceID != in.SourceID {
			continue
		}
		if in.Group != "" && c.Group != in.Group {
			continue
		}
		if len(want) > 0 && !want[c.ID] && !want[c.Slug] {
			continue
		}
		selected = append(selected, c)
	}
	if len(selected) == 0 {
		fail(w, http.StatusBadRequest, "no enabled camera matches the selection")
		return
	}

	s.runs.mu.Lock()
	if s.runs.running() != nil {
		s.runs.mu.Unlock()
		fail(w, http.StatusConflict, "another load test is still running; stop it first")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &loadRun{
		ID:          fmt.Sprintf("lt-%d", time.Now().UnixNano()/1e6),
		Mode:        in.Mode,
		Status:      "running",
		CreatedAt:   time.Now().UTC(),
		Count:       in.Count,
		Concurrency: in.Concurrency,
		TimeoutMS:   in.TimeoutMS,
		DurationSec: in.DurationSec,
		RampSec:     in.RampSec,
		Cameras:     min(len(selected), in.Count),
		Errors:      map[string]int{},
		cancel:      cancel,
	}
	s.runs.runs[run.ID] = run
	s.runs.order = append(s.runs.order, run.ID)
	for len(s.runs.order) > maxRuns {
		delete(s.runs.runs, s.runs.order[0])
		s.runs.order = s.runs.order[1:]
	}
	out := cloneRun(run, false)
	s.runs.mu.Unlock()

	_ = s.st.AddEvent(r.Context(), "info", "loadtest", run.ID,
		fmt.Sprintf("%s load test started: %d clients on %d cameras", run.Mode, run.Count, run.Cameras))
	go s.runLoadtest(ctx, run, selected)
	created(w, out)
}

type target struct {
	slug, user, pass string
}

func (s *Server) runLoadtest(ctx context.Context, run *loadRun, cams []model.Camera) {
	defer run.cancel()
	targets := make([]target, len(cams))
	for i, c := range cams {
		pass, _ := s.box.Decrypt(c.PassEnc)
		targets[i] = target{slug: c.Slug, user: c.ReadUser, pass: pass}
	}

	start := time.Now()
	results := make(chan clientResult, run.Count)
	lives := make([]*rtsp.Live, run.Count)
	for i := range lives {
		lives[i] = &rtsp.Live{}
	}

	var wg sync.WaitGroup
	timeout := time.Duration(run.TimeoutMS) * time.Millisecond
	if run.Mode == "stream" {
		for i := 0; i < run.Count; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if run.RampSec > 0 && run.Count > 1 {
					delay := time.Duration(float64(run.RampSec) * float64(time.Second) * float64(i) / float64(run.Count))
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						return
					}
				}
				if ctx.Err() != nil {
					return
				}
				s.runs.mu.Lock()
				run.Started++
				s.runs.mu.Unlock()
				tg := targets[i%len(targets)]
				res := rtsp.Play(ctx, rtsp.PlayOptions{
					URL: s.cfg.MediaRTSP + "/" + tg.slug, User: tg.user, Pass: tg.pass,
					Duration: time.Duration(run.DurationSec) * time.Second, Timeout: timeout, Live: lives[i],
				})
				msg := ""
				if !res.OK {
					msg = res.Phase + ": " + res.Error
				}
				results <- clientResult{slug: tg.slug, ok: res.OK, err: msg, res: res}
			}(i)
		}
	} else {
		jobs := make(chan int)
		for w := 0; w < run.Concurrency; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					tg := targets[i%len(targets)]
					s.runs.mu.Lock()
					run.Started++
					s.runs.mu.Unlock()
					cctx, cancel := context.WithTimeout(ctx, timeout)
					d := rtsp.DescribeWithAuth(cctx, s.cfg.MediaRTSP+"/"+tg.slug, tg.user, tg.pass, timeout)
					cancel()
					msg := ""
					if !d.OK {
						msg = d.Error
						if msg == "" {
							msg = fmt.Sprintf("status %d", d.Status)
						}
					}
					results <- clientResult{slug: tg.slug, ok: d.OK, err: msg, desc: d}
				}
			}()
		}
		go func() {
			defer close(jobs)
			for i := 0; i < run.Count; i++ {
				select {
				case jobs <- i:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	var (
		all      []clientResult
		tick     = time.NewTicker(time.Second)
		prevB    = make([]uint64, run.Count)
		prevFrm  uint64
		prevLost uint64
		prevStl  uint64
		stopped  = false
	)
	defer tick.Stop()
	drain := true
	for drain {
		select {
		case r, open := <-results:
			if !open {
				drain = false
				break
			}
			all = append(all, r)
			s.runs.mu.Lock()
			run.Done++
			if r.ok {
				run.OK++
			} else {
				run.Fail++
				run.Errors[r.err]++
				if len(run.Samples) < 10 {
					run.Samples = append(run.Samples, r.err)
				}
				if r.res.Ended == "dropped" {
					run.Dropped++
				}
			}
			s.runs.mu.Unlock()
		case <-tick.C:
			stopped = ctx.Err() != nil
			s.sampleRun(run, lives, prevB, &prevFrm, &prevLost, &prevStl, start)
		}
	}
	stopped = stopped || ctx.Err() != nil
	s.sampleRun(run, lives, prevB, &prevFrm, &prevLost, &prevStl, start)

	s.runs.mu.Lock()
	s.finalizeRun(run, all, lives)
	fin := time.Now().UTC()
	run.Status = "done"
	run.Stopped = stopped
	run.FinishedAt = &fin
	run.Active = 0
	run.DurationMS = time.Since(start).Milliseconds()
	run.ElapsedMS = run.DurationMS
	msg := fmt.Sprintf("%s load test finished: %d ok, %d failed", run.Mode, run.OK, run.Fail)
	s.runs.mu.Unlock()
	_ = s.st.AddEvent(context.Background(), "info", "loadtest", run.ID, msg)
}

// sampleRun takes the one-second sample of a running test.
func (s *Server) sampleRun(run *loadRun, lives []*rtsp.Live, prevB []uint64, prevFrm, prevLost, prevStl *uint64, start time.Time) {
	var (
		bytesNow, frames, lost, stalls uint64
		active                         int
		perClient                      []float64
		ttfb, hs, st                   []float64
	)
	for i, l := range lives {
		b := l.Bytes.Load()
		bytesNow += b
		frames += l.Frames.Load()
		lost += l.Lost.Load()
		stalls += l.Stalls.Load()
		if l.Playing.Load() {
			active++
			perClient = append(perClient, float64(b-prevB[i])*8/1000)
		}
		prevB[i] = b
		if v := l.TTFB.Load(); v > 0 {
			ttfb = append(ttfb, float64(v-1))
		}
		if v := l.Handshake.Load(); v > 0 {
			hs = append(hs, float64(v-1))
		}
		if v := l.Start.Load(); v > 0 {
			st = append(st, float64(v-1))
		}
	}
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	lastBytes := run.TotalBytes // previous sample (0 before the first)
	run.TotalBytes = bytesNow
	run.TotalFrames = frames
	run.TotalLost = lost
	run.Stalls = stalls
	el := time.Since(start)
	run.ElapsedMS = el.Milliseconds()
	pt := loadPoint{T: int(el.Seconds() + 0.5), Active: active, Fail: run.Fail,
		Mbps: float64(bytesNow-lastBytes) * 8 / 1e6, FPS: float64(frames - *prevFrm),
		Lost: lost - *prevLost, Stalls: stalls - *prevStl}
	*prevFrm, *prevLost, *prevStl = frames, lost, stalls
	if len(perClient) > 0 {
		sort.Float64s(perClient)
		var sum float64
		for _, v := range perClient {
			sum += v
		}
		pt.AvgKbps = sum / float64(len(perClient))
		pt.P5Kbps = perClient[int(float64(len(perClient)-1)*0.05)]
	}
	if run.Mode == "describe" {
		pt.Active = run.Started - run.Done
		if pt.Active < 0 {
			pt.Active = 0
		}
	}
	if len(run.Series) < 4000 {
		run.Series = append(run.Series, pt)
	}
	if pt.Mbps > run.PeakMbps {
		run.PeakMbps = pt.Mbps
	}
	run.Active = pt.Active
	if run.Mode == "stream" {
		run.TTFB, run.Handshake, run.Start = dist(ttfb), dist(hs), dist(st)
		var pk uint64
		for _, l := range lives {
			pk += l.Packets.Load()
		}
		if pk+lost > 0 {
			run.LossPct = float64(lost) / float64(pk+lost) * 100
		}
	}
}

// finalizeRun computes the end-of-run aggregates. Caller holds runs.mu.
func (s *Server) finalizeRun(run *loadRun, all []clientResult, lives []*rtsp.Live) {
	var (
		hs, desc, ttfb, start, kbps, fps, jit, gap, delay []float64
		pk, lost, frames, bytes                           uint64
		stalled                                           int
	)
	per := map[string]*camStat{}
	ttfbSum := map[string]float64{}
	ttfbN := map[string]int{}
	lossSum := map[string]float64{}
	for _, r := range all {
		cs := per[r.slug]
		if cs == nil {
			cs = &camStat{Slug: r.slug}
			per[r.slug] = cs
		}
		cs.Clients++
		if r.ok {
			cs.OK++
		} else {
			cs.Fail++
		}
		if run.Mode == "describe" {
			hs = append(hs, float64(r.desc.LatencyMS))
			desc = append(desc, float64(r.desc.LatencyMS))
			continue
		}
		p := r.res
		if p.HandshakeMS > 0 {
			hs = append(hs, float64(p.HandshakeMS))
			desc = append(desc, float64(p.DescribeMS))
		}
		pk += p.Packets
		lost += p.Lost
		frames += p.Frames
		bytes += p.Bytes
		if p.Stalls > 0 {
			stalled++
		}
		if p.Packets > 0 {
			ttfb = append(ttfb, float64(p.TTFBMS))
			start = append(start, float64(p.StartMS))
			ttfbSum[r.slug] += float64(p.TTFBMS)
			ttfbN[r.slug]++
			lossSum[r.slug] += p.LossPct
		}
		if p.AvgKbps > 0 {
			kbps = append(kbps, p.AvgKbps)
			cs.AvgKbps += p.AvgKbps
			fps = append(fps, p.FPS)
			jit = append(jit, p.JitterMS)
			gap = append(gap, float64(p.MaxGapMS))
		}
		if p.DelaySamples > 0 {
			delay = append(delay, p.DelayAvgMS)
		}
	}
	run.Handshake, run.Describe = dist(hs), dist(desc)
	if run.Mode == "describe" {
		d := run.Handshake
		run.AvgMS, run.P50MS, run.P95MS, run.MaxMS = int64(d.Avg), int64(d.P50), int64(d.P95), int64(d.Max)
	} else {
		d := run.Handshake
		run.AvgMS, run.P50MS, run.P95MS, run.MaxMS = int64(d.Avg), int64(d.P50), int64(d.P95), int64(d.Max)
		run.TTFB, run.Start = dist(ttfb), dist(start)
		run.Bitrate, run.FPS, run.Jitter, run.MaxGap, run.Delay = dist(kbps), dist(fps), dist(jit), dist(gap), dist(delay)
		run.TotalPackets, run.TotalLost, run.TotalFrames, run.TotalBytes = pk, lost, frames, bytes
		if pk+lost > 0 {
			run.LossPct = float64(lost) / float64(pk+lost) * 100
		}
		run.StalledClients = stalled
		var st uint64
		for _, l := range lives {
			st += l.Stalls.Load()
		}
		run.Stalls = st
		var sum float64
		n := 0
		for _, p := range run.Series {
			if p.Mbps > 0 {
				sum += p.Mbps
				n++
			}
		}
		if n > 0 {
			run.AvgMbps = sum / float64(n)
		}
	}
	list := make([]camStat, 0, len(per))
	for slug, cs := range per {
		if cs.OK > 0 {
			cs.AvgKbps /= float64(cs.OK)
		}
		if n := ttfbN[slug]; n > 0 {
			cs.AvgTTFB = ttfbSum[slug] / float64(n)
			cs.Loss = lossSum[slug] / float64(n)
		}
		list = append(list, *cs)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Fail != list[j].Fail {
			return list[i].Fail > list[j].Fail
		}
		return strings.Compare(list[i].Slug, list[j].Slug) < 0
	})
	if len(list) > 100 {
		list = list[:100]
	}
	run.PerCamera = list
}

func (s *Server) handleGetLoadtest(w http.ResponseWriter, r *http.Request) {
	run, found := s.runs.get(r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, "run not found")
		return
	}
	ok(w, run)
}

func (s *Server) handleListLoadtests(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"items": s.runs.list()})
}

func (s *Server) handleStopLoadtest(w http.ResponseWriter, r *http.Request) {
	s.runs.mu.Lock()
	run, found := s.runs.runs[r.PathValue("id")]
	if !found {
		s.runs.mu.Unlock()
		fail(w, http.StatusNotFound, "run not found")
		return
	}
	cancel := run.cancel
	status := run.Status
	s.runs.mu.Unlock()
	if status == "running" && cancel != nil {
		cancel()
	}
	ok(w, map[string]any{"id": run.ID, "stopping": status == "running"})
}
