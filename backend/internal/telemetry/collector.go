// Package telemetry samples the media server once a second and keeps in-memory
// time series (global and per path) for the real-time dashboard.
//
// What is measured, and where each number comes from:
//
//   - Egress / ingest / per-camera bandwidth: byte-counter deltas of the MediaMTX
//     /v3/paths/list endpoint (outboundBytes / inboundBytes) divided by the real
//     time between two polls.
//   - Quality (loss, discards, jitter): RTSP session counters of /v3/rtspsessions/list
//     (RTP packets sent, loss reported by clients' RTCP receiver reports, packets the
//     server discarded because a reader was too slow, publisher-side jitter).
//   - Responsiveness: round-trip of the Control API call itself and of an RTSP
//     OPTIONS probe against the RTSP port (no auth, no media, no side effects).
//   - Host: NIC tx/rx, CPU and memory from /proc (Linux only).
package telemetry

import (
	"context"
	"log/slog"
	"net"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aclab/restreamcamera/internal/mediamtx"
	"github.com/aclab/restreamcamera/internal/rtsp"
)

const (
	fineCap      = 900  // 15 min at 1 s
	coarseCap    = 2880 // 24 h at 30 s
	coarseEvery  = 30   // ticks per coarse bucket
	forgetAfter  = 25 * time.Hour
	sessionLimit = 200 // sessions kept per path for the detail view
)

// Options configures the collector.
type Options struct {
	Interval     time.Duration // poll period, default 1s
	CameraPrefix string        // fallback classifier when Cameras is nil
	SourcePrefix string
	// Cameras returns the slugs of every virtual camera (slugs are user chosen, so a
	// prefix cannot identify them). It is refreshed every few seconds.
	Cameras      func(context.Context) ([]string, error)
	RTSPAddr     string // host:port used by the OPTIONS probe ("" disables it)
	SessionEvery int    // poll RTSP sessions every N ticks (default 2)
	ProbeEvery   int    // RTSP probe every N ticks (default 5)
}

// Point is one sample of one path.
type Point struct {
	TS      int32
	Out, In float32 // kbps
	Readers uint16
	Loss    float32 // % of packets lost/discarded in this sample window
}

// GPoint is one global sample.
type GPoint struct {
	TS       int32
	Out, In  float32 // egress to clients, ingest from sources (kbps)
	Loop     float32 // internal proxy bandwidth between origin and virtual cameras (kbps)
	Readers  uint16
	Ready    uint16
	NicTx    float32 // kbps
	NicRx    float32 // kbps
	CPU      float32 // %
	APIms    float32 // Control API round trip
	RTSPms   float32 // RTSP OPTIONS round trip, -1 when not measured
	Loss     float32 // %
	Sessions uint16
}

type hostRaw struct {
	rx, tx             uint64
	cpuTotal, cpuIdle  uint64
	memTotal, memAvail uint64
	load1              float64
}

// SessionView is one reader of a camera path.
type SessionView struct {
	ID        string    `json:"id"`
	Remote    string    `json:"remote"`
	Agent     string    `json:"agent"`
	Transport string    `json:"transport"`
	Created   time.Time `json:"created"`
	Kbps      float64   `json:"kbps"`
	Bytes     uint64    `json:"bytes"`
	Packets   uint64    `json:"packets"`
	LossPct   float64   `json:"lossPct"`
	Discarded uint64    `json:"discarded"`
	Internal  bool      `json:"internal"`
}

// PathLive is the current state of one path.
type PathLive struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind"` // camera | source | other
	Ready     bool             `json:"ready"`
	Since     time.Time        `json:"since"`
	Tracks    []mediamtx.Track `json:"tracks"`
	Readers   int              `json:"readers"`
	ReadersBy map[string]int   `json:"readersBy"`
	OutKbps   float64          `json:"outKbps"`
	InKbps    float64          `json:"inKbps"`
	OutBytes  uint64           `json:"outBytes"`
	InBytes   uint64           `json:"inBytes"`
	PeakOut   float64          `json:"peakOut"`
	AvgOut    float64          `json:"avgOut"` // mean of the last minute
	// quality (RTSP sessions)
	Sessions    int     `json:"sessions"`
	LossPct     float64 `json:"lossPct"`     // last window
	LossTotal   float64 `json:"lossTotal"`   // since the path appeared
	Discarded   uint64  `json:"discarded"`   // packets dropped for slow readers, cumulative
	PacketsSent uint64  `json:"packetsSent"` // cumulative
	LostTotal   uint64  `json:"lostTotal"`
	InJitterMS  float64 `json:"inJitterMs"` // publisher-side RTP jitter (RTP clock units, ~ms at 1 kHz)
	InLossPct   float64 `json:"inLossPct"`
	FramesErr   uint64  `json:"framesInError"`
}

// HostLive is the machine view (Linux only).
type HostLive struct {
	Available bool    `json:"available"`
	NicTx     float64 `json:"nicTxKbps"`
	NicRx     float64 `json:"nicRxKbps"`
	CPU       float64 `json:"cpuPct"`
	MemPct    float64 `json:"memPct"`
	MemTotal  uint64  `json:"memTotal"`
	Load1     float64 `json:"load1"`
}

// Live is the current global state.
type Live struct {
	TS          time.Time      `json:"ts"`
	MediaOnline bool           `json:"mediaOnline"`
	LastError   string         `json:"lastError"`
	OutKbps     float64        `json:"outKbps"`
	InKbps      float64        `json:"inKbps"`
	LoopKbps    float64        `json:"loopKbps"`
	Readers     int            `json:"readers"`
	ReadersBy   map[string]int `json:"readersBy"`
	Active      int            `json:"activeCameras"` // camera paths with media
	Sessions    int            `json:"sessions"`
	LossPct     float64        `json:"lossPct"`
	APIms       float64        `json:"apiMs"`
	RTSPms      float64        `json:"rtspMs"`
	PeakOut     float64        `json:"peakOutKbps"`
	PeakOutAt   time.Time      `json:"peakOutAt"`
	PeakReaders int            `json:"peakReaders"`
	TotalOut    uint64         `json:"totalOutBytes"`
	TotalIn     uint64         `json:"totalInBytes"`
	Host        HostLive       `json:"host"`
	Heap        uint64         `json:"heapBytes"`
	Goroutines  int            `json:"goroutines"`
	Uptime      int64          `json:"collectorUptimeSec"`
}

type pathSeries struct {
	fine   *ring[Point]
	coarse *ring[Point]
	n      int
	out    float64
	in     float64
	loss   float64
	readMx uint16
	seen   time.Time
}

type counters struct {
	in, out uint64
	at      time.Time
}

type sessPrev struct {
	pk, lost, disc, bytes uint64
	at                    time.Time
}

type gAccum struct {
	n                          int
	out, in, loop, tx, rx, cpu float64
	api, rtsp, loss            float64
	rtspN                      int
	readers, ready, sessions   uint16
}

type pathAgg struct {
	pk, lost, disc uint64
	peak           float64
	recent         []float64 // last 60 egress samples
}

// Collector owns the sampling loop and all series.
type Collector struct {
	mtx *mediamtx.Client
	log *slog.Logger
	opt Options

	mu       sync.RWMutex
	camSet   map[string]bool // nil until the first successful refresh
	started  time.Time
	tick     int64
	lastTS   int32
	prev     map[string]counters
	prevSess map[string]sessPrev
	agg      map[string]*pathAgg
	paths    map[string]*pathSeries
	live     Live
	liveP    map[string]*PathLive
	sess     map[string][]SessionView
	gFine    *ring[GPoint]
	gCoarse  *ring[GPoint]
	gAcc     gAccum
	hostPrev hostRaw
	hostAt   time.Time
	hostOK   bool
	nicTx    float64
	nicRx    float64
	cpuPct   float64
	probeMS  float64
	sessLoss float64
	sessN    int
	peakOut  float64
	peakAt   time.Time
	peakRd   int
	ms       runtime.MemStats
	msAt     time.Time
}

// New creates a collector.
func New(mtx *mediamtx.Client, log *slog.Logger, opt Options) *Collector {
	if opt.Interval <= 0 {
		opt.Interval = time.Second
	}
	if opt.SessionEvery <= 0 {
		opt.SessionEvery = 2
	}
	if opt.ProbeEvery <= 0 {
		opt.ProbeEvery = 5
	}
	return &Collector{
		mtx: mtx, log: log, opt: opt, started: time.Now(),
		prev:     map[string]counters{},
		prevSess: map[string]sessPrev{},
		agg:      map[string]*pathAgg{},
		paths:    map[string]*pathSeries{},
		liveP:    map[string]*PathLive{},
		sess:     map[string][]SessionView{},
		gFine:    newRing[GPoint](fineCap),
		gCoarse:  newRing[GPoint](coarseCap),
		probeMS:  -1,
	}
}

// RTSPHostPort extracts host:port from an rtsp:// base URL.
func RTSPHostPort(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(u.Host); err != nil {
		return net.JoinHostPort(u.Host, "554")
	}
	return u.Host
}

// Run polls until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.opt.Interval)
	defer t.Stop()
	c.Poll(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			c.Poll(ctx, now)
		}
	}
}

func (c *Collector) kind(name string) string {
	if c.opt.SourcePrefix != "" && strings.HasPrefix(name, c.opt.SourcePrefix) {
		return "source"
	}
	if c.camSet != nil {
		if c.camSet[name] {
			return "camera"
		}
		return "other"
	}
	if c.opt.CameraPrefix != "" && strings.HasPrefix(name, c.opt.CameraPrefix) {
		return "camera"
	}
	return "other"
}

func readerGroup(t string) string {
	switch t {
	case "rtspSession", "rtspsSession":
		return "rtsp"
	case "webRTCSession":
		return "webrtc"
	case "hlsMuxer":
		return "hls"
	case "srtConn":
		return "srt"
	}
	return "other"
}

func isLoopback(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	h = strings.Trim(h, "[]")
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Poll takes one sample (exported so tests can drive time).
func (c *Collector) Poll(ctx context.Context, now time.Time) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	t0 := time.Now()
	paths, err := c.mtx.ListPaths(cctx)
	apiMS := float64(time.Since(t0).Microseconds()) / 1000

	// Every network call happens before the lock so readers are never blocked on I/O.
	next := c.tick + 1 // only Poll writes c.tick
	var sessions []mediamtx.RTSPSession
	haveSessions := false
	if err == nil && (next == 1 || next%int64(c.opt.SessionEvery) == 0) {
		if ss, serr := c.mtx.ListRTSPSessions(cctx); serr == nil {
			sessions, haveSessions = ss, true
		}
	}
	probeMS, probed := 0.0, false
	if err == nil && c.opt.RTSPAddr != "" && next%int64(c.opt.ProbeEvery) == 0 {
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
		if ms, perr := rtsp.Ping(pctx, c.opt.RTSPAddr, 3*time.Second); perr == nil {
			probeMS = ms
		} else {
			probeMS = 3000 // timed out or refused: surfaces as a spike
		}
		probed = true
		pcancel()
	}

	var slugs []string
	haveSlugs := false
	if err == nil && c.opt.Cameras != nil && (next == 1 || next%5 == 0) {
		if l, lerr := c.opt.Cameras(cctx); lerr == nil {
			slugs, haveSlugs = l, true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick++
	first := c.tick == 1
	if haveSlugs {
		set := make(map[string]bool, len(slugs))
		for _, n := range slugs {
			set[n] = true
		}
		c.camSet = set
	}

	if err != nil {
		c.live.MediaOnline = false
		c.live.LastError = err.Error()
		c.live.TS = now.UTC()
		return
	}
	c.live.MediaOnline = true
	c.live.LastError = ""
	if haveSessions {
		c.ingestSessions(sessions, now)
	}
	if probed {
		c.probeMS = probeMS
	}

	ts := int32(now.Unix())
	if ts <= c.lastTS {
		ts = c.lastTS + 1
	}
	c.lastTS = ts

	var (
		egressCam, srcOut, camIn, srcIn float64
		totOut, totIn                   uint64
		readers, active                 int
		readersBy                       = map[string]int{}
		seenNow                         = make(map[string]bool, len(paths))
	)
	for _, p := range paths {
		kind := c.kind(p.Name)
		seenNow[p.Name] = true
		pc, had := c.prev[p.Name]
		var outK, inK float64
		if !first {
			dt := now.Sub(pc.at).Seconds()
			if !had || dt <= 0 {
				dt = c.opt.Interval.Seconds()
			}
			dIn, dOut := p.InboundBytes, p.OutboundBytes
			if had && p.InboundBytes >= pc.in {
				dIn = p.InboundBytes - pc.in
			}
			if had && p.OutboundBytes >= pc.out {
				dOut = p.OutboundBytes - pc.out
			}
			inK = float64(dIn) * 8 / 1000 / dt
			outK = float64(dOut) * 8 / 1000 / dt
		}
		c.prev[p.Name] = counters{in: p.InboundBytes, out: p.OutboundBytes, at: now}

		rd := len(p.Readers)
		by := map[string]int{}
		for _, r := range p.Readers {
			by[readerGroup(r.Type)]++
		}
		switch kind {
		case "camera":
			egressCam += outK
			camIn += inK
			totOut += p.OutboundBytes
			for k, v := range by {
				readersBy[k] += v
			}
			readers += rd
			if p.Ready || p.Available {
				active++
			}
		case "source":
			srcOut += outK
			srcIn += inK
			totIn += p.InboundBytes
			// readers of an origin path are the internal pulls of virtual cameras,
			// except in redirect mode where real clients read it directly.
		}

		a := c.agg[p.Name]
		if a == nil {
			a = &pathAgg{}
			c.agg[p.Name] = a
		}
		if outK > a.peak {
			a.peak = outK
		}
		a.recent = append(a.recent, outK)
		if len(a.recent) > 60 {
			a.recent = a.recent[len(a.recent)-60:]
		}
		var avg float64
		for _, v := range a.recent {
			avg += v
		}
		avg /= float64(len(a.recent))

		pl := c.liveP[p.Name]
		if pl == nil {
			pl = &PathLive{Name: p.Name}
			c.liveP[p.Name] = pl
		}
		pl.Kind, pl.Ready, pl.Since = kind, p.Ready || p.Available, p.ReadyTime
		pl.Tracks, pl.Readers, pl.ReadersBy = p.Tracks2, rd, by
		pl.OutKbps, pl.InKbps, pl.OutBytes, pl.InBytes = outK, inK, p.OutboundBytes, p.InboundBytes
		pl.PeakOut, pl.AvgOut = a.peak, avg
		pl.FramesErr = p.InboundFramesInError

		ps := c.paths[p.Name]
		if ps == nil {
			ps = &pathSeries{fine: newRing[Point](fineCap), coarse: newRing[Point](coarseCap)}
			c.paths[p.Name] = ps
		}
		loss := c.windowLoss(p.Name)
		pt := Point{TS: ts, Out: float32(outK), In: float32(inK), Readers: uint16(min(rd, 65535)), Loss: float32(loss)}
		ps.fine.push(pt)
		ps.n++
		ps.out += outK
		ps.in += inK
		ps.loss += loss
		if pt.Readers > ps.readMx {
			ps.readMx = pt.Readers
		}
		ps.seen = now
	}

	// A path that vanished (on-demand source stopped) restarts its counters next time.
	for name := range c.prev {
		if !seenNow[name] {
			delete(c.prev, name)
			if pl := c.liveP[name]; pl != nil {
				pl.Ready, pl.Readers, pl.OutKbps, pl.InKbps, pl.Sessions = false, 0, 0, 0, 0
				pl.ReadersBy = map[string]int{}
			}
			delete(c.sess, name)
			if a := c.agg[name]; a != nil {
				a.recent = a.recent[:0]
			}
		}
	}

	// client egress = camera paths + direct reads of origin paths that are not internal pulls
	direct := srcOut - camIn
	if direct < 0 {
		direct = 0
	}
	egress := egressCam + direct
	ingest := srcIn
	loop := camIn

	if egress > c.peakOut {
		c.peakOut, c.peakAt = egress, now.UTC()
	}
	if readers > c.peakRd {
		c.peakRd = readers
	}

	c.sampleHost(now)
	g := GPoint{
		TS: ts, Out: float32(egress), In: float32(ingest), Loop: float32(loop),
		Readers: uint16(min(readers, 65535)), Ready: uint16(min(active, 65535)),
		NicTx: float32(c.nicTx), NicRx: float32(c.nicRx), CPU: float32(c.cpuPct),
		APIms: float32(apiMS), RTSPms: float32(c.probeMS), Loss: float32(c.sessLoss),
		Sessions: uint16(min(c.sessN, 65535)),
	}
	c.gFine.push(g)
	a := &c.gAcc
	a.n++
	a.out += egress
	a.in += ingest
	a.loop += loop
	a.tx += c.nicTx
	a.rx += c.nicRx
	a.cpu += c.cpuPct
	a.api += apiMS
	a.loss += c.sessLoss
	if c.probeMS >= 0 {
		a.rtsp += c.probeMS
		a.rtspN++
	}
	if g.Readers > a.readers {
		a.readers = g.Readers
	}
	a.ready, a.sessions = g.Ready, g.Sessions

	if c.tick%coarseEvery == 0 {
		c.flushCoarse(ts)
	}

	if now.Sub(c.msAt) > 5*time.Second {
		runtime.ReadMemStats(&c.ms)
		c.msAt = now
	}
	c.live = Live{
		TS: now.UTC(), MediaOnline: true, OutKbps: egress, InKbps: ingest, LoopKbps: loop,
		Readers: readers, ReadersBy: readersBy, Active: active, Sessions: c.sessN, LossPct: c.sessLoss,
		APIms: apiMS, RTSPms: c.probeMS, PeakOut: c.peakOut, PeakOutAt: c.peakAt, PeakReaders: c.peakRd,
		TotalOut: totOut, TotalIn: totIn,
		Host: HostLive{Available: c.hostOK, NicTx: c.nicTx, NicRx: c.nicRx, CPU: c.cpuPct,
			MemPct: c.memPct(), MemTotal: c.hostPrev.memTotal, Load1: c.hostPrev.load1},
		Heap: c.ms.HeapAlloc, Goroutines: runtime.NumGoroutine(), Uptime: int64(time.Since(c.started).Seconds()),
	}

	// forget series of paths that have been gone for a day
	if c.tick%300 == 0 {
		for name, ps := range c.paths {
			if now.Sub(ps.seen) > forgetAfter {
				delete(c.paths, name)
				delete(c.liveP, name)
				delete(c.agg, name)
			}
		}
	}
}

func (c *Collector) memPct() float64 {
	h := c.hostPrev
	if h.memTotal == 0 {
		return 0
	}
	return float64(h.memTotal-h.memAvail) / float64(h.memTotal) * 100
}

func (c *Collector) flushCoarse(ts int32) {
	a := &c.gAcc
	if a.n > 0 {
		n := float32(a.n)
		g := GPoint{
			TS: ts, Out: float32(a.out) / n, In: float32(a.in) / n, Loop: float32(a.loop) / n,
			Readers: a.readers, Ready: a.ready, NicTx: float32(a.tx) / n, NicRx: float32(a.rx) / n,
			CPU: float32(a.cpu) / n, APIms: float32(a.api) / n, RTSPms: -1, Loss: float32(a.loss) / n, Sessions: a.sessions,
		}
		if a.rtspN > 0 {
			g.RTSPms = float32(a.rtsp / float64(a.rtspN))
		}
		c.gCoarse.push(g)
	}
	*a = gAccum{}
	for _, ps := range c.paths {
		if ps.n == 0 {
			continue
		}
		n := float32(ps.n)
		ps.coarse.push(Point{TS: ts, Out: float32(ps.out) / n, In: float32(ps.in) / n, Readers: ps.readMx, Loss: float32(ps.loss) / n})
		ps.n, ps.out, ps.in, ps.loss, ps.readMx = 0, 0, 0, 0, 0
	}
}

func (c *Collector) sampleHost(now time.Time) {
	h, ok := readHost()
	c.hostOK = ok
	if !ok {
		return
	}
	if !c.hostAt.IsZero() {
		if dt := now.Sub(c.hostAt).Seconds(); dt > 0 {
			if h.tx >= c.hostPrev.tx {
				c.nicTx = float64(h.tx-c.hostPrev.tx) * 8 / 1000 / dt
			}
			if h.rx >= c.hostPrev.rx {
				c.nicRx = float64(h.rx-c.hostPrev.rx) * 8 / 1000 / dt
			}
			if dTot := h.cpuTotal - c.hostPrev.cpuTotal; dTot > 0 && h.cpuTotal >= c.hostPrev.cpuTotal {
				c.cpuPct = (1 - float64(h.cpuIdle-c.hostPrev.cpuIdle)/float64(dTot)) * 100
			}
		}
	}
	c.hostPrev, c.hostAt = h, now
}

// windowLoss returns the loss % of the last session window for a path (set by ingestSessions).
func (c *Collector) windowLoss(name string) float64 {
	if pl := c.liveP[name]; pl != nil {
		return pl.LossPct
	}
	return 0
}

// ingestSessions folds RTSP session counters into per-path quality figures.
func (c *Collector) ingestSessions(list []mediamtx.RTSPSession, now time.Time) {
	type win struct {
		sessions          int
		dPk, dLost, dDisc uint64
		inJit             float64
		inJitN            int
		inPk, inLost      uint64
	}
	wins := map[string]*win{}
	views := map[string][]SessionView{}
	nextPrev := make(map[string]sessPrev, len(list))
	var gPk, gBad uint64
	reads := 0

	for _, s := range list {
		w := wins[s.Path]
		if w == nil {
			w = &win{}
			wins[s.Path] = w
		}
		if s.State == "publish" {
			w.inJit += s.InboundRTPPacketsJitter
			w.inJitN++
			w.inPk += s.InboundRTPPackets
			w.inLost += s.InboundRTPPacketsLost
			continue
		}
		if s.State != "read" {
			continue
		}
		w.sessions++
		reads++
		pv, had := c.prevSess[s.ID]
		pk, lost, disc, by := s.OutboundRTPPackets, s.OutboundRTPPacketsReportedLost, s.OutboundRTPPacketsDiscarded, s.OutboundBytes
		var dPk, dLost, dDisc, dBy uint64
		if had && pk >= pv.pk {
			dPk, dLost, dDisc, dBy = pk-pv.pk, sub(lost, pv.lost), sub(disc, pv.disc), sub(by, pv.bytes)
		} else {
			dPk, dLost, dDisc, dBy = pk, lost, disc, by
		}
		var kbps float64
		if had {
			if dt := now.Sub(pv.at).Seconds(); dt > 0 {
				kbps = float64(dBy) * 8 / 1000 / dt
			}
		}
		w.dPk += dPk
		w.dLost += dLost
		w.dDisc += dDisc
		gPk += dPk
		gBad += dLost + dDisc
		nextPrev[s.ID] = sessPrev{pk: pk, lost: lost, disc: disc, bytes: by, at: now}

		if len(views[s.Path]) < sessionLimit {
			lp := 0.0
			if tot := pk + lost + disc; tot > 0 {
				lp = float64(lost+disc) / float64(tot) * 100
			}
			views[s.Path] = append(views[s.Path], SessionView{
				ID: s.ID, Remote: s.RemoteAddr, Agent: s.UserAgent, Transport: s.Transport, Created: s.Created,
				Kbps: kbps, Bytes: by, Packets: pk, LossPct: lp, Discarded: disc,
				Internal: isLoopback(s.RemoteAddr) && c.kind(s.Path) == "source",
			})
		}
	}
	c.prevSess = nextPrev
	c.sess = views
	c.sessN = reads
	if gPk+gBad > 0 {
		c.sessLoss = float64(gBad) / float64(gPk+gBad) * 100
	} else {
		c.sessLoss = 0
	}

	for name, w := range wins {
		pl := c.liveP[name]
		if pl == nil {
			continue
		}
		a := c.agg[name]
		if a == nil {
			a = &pathAgg{}
			c.agg[name] = a
		}
		a.pk += w.dPk
		a.lost += w.dLost
		a.disc += w.dDisc
		pl.Sessions = w.sessions
		pl.LossPct = 0
		if t := w.dPk + w.dLost + w.dDisc; t > 0 {
			pl.LossPct = float64(w.dLost+w.dDisc) / float64(t) * 100
		}
		if t := a.pk + a.lost + a.disc; t > 0 {
			pl.LossTotal = float64(a.lost+a.disc) / float64(t) * 100
		}
		pl.Discarded, pl.PacketsSent, pl.LostTotal = a.disc, a.pk, a.lost
		if w.inJitN > 0 {
			pl.InJitterMS = w.inJit / float64(w.inJitN)
		}
		if t := w.inPk + w.inLost; t > 0 {
			pl.InLossPct = float64(w.inLost) / float64(t) * 100
		}
	}
	// paths that have no session this round
	for name, pl := range c.liveP {
		if _, ok := wins[name]; !ok {
			pl.Sessions, pl.LossPct = 0, 0
		}
	}
}

func sub(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return a - b
}

// ------------------------------------------------------------------ queries

// Snapshot returns the current global state.
func (c *Collector) Snapshot() Live {
	c.mu.RLock()
	defer c.mu.RUnlock()
	l := c.live
	l.ReadersBy = cloneInts(l.ReadersBy)
	if l.TS.IsZero() {
		l.TS = time.Now().UTC()
	}
	return l
}

// Path returns the live state of one path.
func (c *Collector) Path(name string) (PathLive, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.liveP[name]
	if !ok {
		return PathLive{}, false
	}
	return clonePath(p), true
}

// Paths returns a copy of every known live path.
func (c *Collector) Paths() map[string]PathLive {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]PathLive, len(c.liveP))
	for k, v := range c.liveP {
		out[k] = clonePath(v)
	}
	return out
}

// Sessions returns the readers currently attached to a path.
func (c *Collector) Sessions(name string) []SessionView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]SessionView(nil), c.sess[name]...)
}

func clonePath(p *PathLive) PathLive {
	cp := *p
	cp.ReadersBy = cloneInts(p.ReadersBy)
	cp.Tracks = append([]mediamtx.Track(nil), p.Tracks...)
	return cp
}

func cloneInts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// GlobalSeries is a column-oriented global time series.
type GlobalSeries struct {
	Step    int       `json:"step"` // seconds between points
	TS      []int32   `json:"ts"`
	Out     []float32 `json:"out"`
	In      []float32 `json:"in"`
	Loop    []float32 `json:"loop"`
	Readers []uint16  `json:"readers"`
	Ready   []uint16  `json:"ready"`
	NicTx   []float32 `json:"nicTx"`
	NicRx   []float32 `json:"nicRx"`
	CPU     []float32 `json:"cpu"`
	APIms   []float32 `json:"apiMs"`
	RTSPms  []float32 `json:"rtspMs"`
	Loss    []float32 `json:"loss"`
}

// PathSeries is the series of one path aligned to a GlobalSeries grid.
type PathSeries struct {
	Out     []float32 `json:"out"`
	In      []float32 `json:"in"`
	Readers []uint16  `json:"readers"`
	Loss    []float32 `json:"loss"`
}

// Series returns the global series for the last rangeSec seconds plus the aligned
// series of the given paths.
func (c *Collector) Series(rangeSec int, names []string) (GlobalSeries, map[string]PathSeries) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if rangeSec <= 0 {
		rangeSec = 300
	}
	fine := rangeSec <= fineCap
	gs := GlobalSeries{Step: 1}
	g := c.gFine
	if !fine {
		gs.Step, g = coarseEvery, c.gCoarse
	}
	var from int32
	if last, ok := g.last(); ok {
		from = last.TS - int32(rangeSec)
	}
	index := map[int32]int{}
	g.each(func(p GPoint) {
		if p.TS <= from {
			return
		}
		index[p.TS] = len(gs.TS)
		gs.TS = append(gs.TS, p.TS)
		gs.Out = append(gs.Out, p.Out)
		gs.In = append(gs.In, p.In)
		gs.Loop = append(gs.Loop, p.Loop)
		gs.Readers = append(gs.Readers, p.Readers)
		gs.Ready = append(gs.Ready, p.Ready)
		gs.NicTx = append(gs.NicTx, p.NicTx)
		gs.NicRx = append(gs.NicRx, p.NicRx)
		gs.CPU = append(gs.CPU, p.CPU)
		gs.APIms = append(gs.APIms, p.APIms)
		gs.RTSPms = append(gs.RTSPms, p.RTSPms)
		gs.Loss = append(gs.Loss, p.Loss)
	})
	out := make(map[string]PathSeries, len(names))
	n := len(gs.TS)
	for _, name := range names {
		ps := c.paths[name]
		s := PathSeries{Out: make([]float32, n), In: make([]float32, n), Readers: make([]uint16, n), Loss: make([]float32, n)}
		if ps != nil {
			r := ps.fine
			if !fine {
				r = ps.coarse
			}
			r.each(func(p Point) {
				if i, ok := index[p.TS]; ok {
					s.Out[i], s.In[i], s.Readers[i], s.Loss[i] = p.Out, p.In, p.Readers, p.Loss
				}
			})
		}
		out[name] = s
	}
	return gs, out
}

// TopByEgress returns camera path names sorted by current egress, highest first.
func (c *Collector) TopByEgress(n int) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	type kv struct {
		name string
		v    float64
	}
	all := make([]kv, 0, len(c.liveP))
	for name, p := range c.liveP {
		if p.Kind == "camera" && (p.OutKbps > 0 || p.Readers > 0) {
			all = append(all, kv{name, p.OutKbps})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].name < all[j].name
	})
	if n > len(all) {
		n = len(all)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = all[i].name
	}
	return out
}
