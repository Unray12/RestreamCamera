package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/telemetry"
)

// SetTelemetry attaches the real-time collector (nil keeps the legacy 5 s numbers).
func (s *Server) SetTelemetry(c *telemetry.Collector) { s.tel = c }

func r1(v float64) float64 { return math.Round(v*10) / 10 }

func r1s(in []float32) []float32 {
	for i, v := range in {
		in[i] = float32(math.Round(float64(v)*10) / 10)
	}
	return in
}

// camRow is one camera with its live numbers, as shown in the dashboard tables.
type camRow struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Group      string `json:"group"`
	Mode       string `json:"mode"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	Enabled    bool   `json:"enabled"`
	OnDemand   bool   `json:"onDemand"`
	State      string `json:"state"`

	Readers   int            `json:"readers"`
	ReadersBy map[string]int `json:"readersBy"`
	OutKbps   float64        `json:"outKbps"`
	AvgOut    float64        `json:"avgOutKbps"`
	PeakOut   float64        `json:"peakOutKbps"`
	InKbps    float64        `json:"inKbps"`
	OutBytes  uint64         `json:"outBytes"`
	PerReader float64        `json:"perReaderKbps"`

	Sessions  int     `json:"sessions"`
	LossPct   float64 `json:"lossPct"`
	LossTotal float64 `json:"lossTotalPct"`
	Discarded uint64  `json:"discarded"`

	Codec  string     `json:"codec"`
	Width  int        `json:"width"`
	Height int        `json:"height"`
	Tracks []string   `json:"tracks"`
	Since  *time.Time `json:"since,omitempty"`
}

func (s *Server) buildRow(c model.Camera, paths map[string]telemetry.PathLive) camRow {
	row := camRow{
		ID: c.ID, Slug: c.Slug, Name: c.Name, Group: c.Group, Mode: c.Mode, SourceID: c.SourceID,
		SourceName: c.SourceName, Enabled: c.Enabled, OnDemand: c.OnDemand, State: c.Runtime.State,
		ReadersBy: map[string]int{}, Tracks: []string{},
	}
	p, ok := paths[c.Slug]
	if !ok {
		if c.Enabled && c.OnDemand && row.State != "error" {
			row.State = "idle"
		}
		return row
	}
	if !c.Enabled {
		row.State = "disabled"
	} else if p.Ready {
		row.State = "ready"
	} else if p.Readers > 0 {
		row.State = "offline"
	} else {
		row.State = "idle"
	}
	row.Readers, row.ReadersBy = p.Readers, p.ReadersBy
	row.OutKbps, row.AvgOut, row.PeakOut, row.InKbps, row.OutBytes = p.OutKbps, p.AvgOut, p.PeakOut, p.InKbps, p.OutBytes
	if p.Readers > 0 {
		row.PerReader = p.OutKbps / float64(p.Readers)
	}
	row.Sessions, row.LossPct, row.LossTotal, row.Discarded = p.Sessions, p.LossPct, p.LossTotal, p.Discarded
	for _, t := range p.Tracks {
		row.Tracks = append(row.Tracks, t.Codec)
		if w, okw := t.CodecProps["width"].(float64); okw && row.Width == 0 {
			row.Codec = t.Codec
			row.Width = int(w)
			if h, okh := t.CodecProps["height"].(float64); okh {
				row.Height = int(h)
			}
		}
	}
	if row.Codec == "" && len(p.Tracks) > 0 {
		row.Codec = p.Tracks[0].Codec
	}
	if !p.Since.IsZero() {
		t := p.Since
		row.Since = &t
	}
	return row
}

func (s *Server) cameraRows(ctx context.Context) ([]camRow, error) {
	cams, err := s.allCameras(ctx)
	if err != nil {
		return nil, err
	}
	var paths map[string]telemetry.PathLive
	if s.tel != nil {
		paths = s.tel.Paths()
	}
	rows := make([]camRow, 0, len(cams))
	for _, c := range cams {
		rows = append(rows, s.buildRow(c, paths))
	}
	return rows, nil
}

// resolveCams turns "a,b,c" or "top:N" into slugs (top-N picks the busiest cameras right now).
func (s *Server) resolveCams(spec string) (names []string, top bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" || s.tel == nil {
		return nil, false
	}
	if strings.HasPrefix(spec, "top:") {
		n, _ := strconv.Atoi(spec[4:])
		if n <= 0 || n > 12 {
			n = 5
		}
		return s.tel.TopByEgress(n), true
	}
	seen := map[string]bool{}
	for _, n := range strings.Split(spec, ",") {
		n = strings.TrimSpace(n)
		if n != "" && !seen[n] && len(names) < 24 {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names, false
}

// handleMonitorOverview: everything the dashboard header and KPI strip need.
func (s *Server) handleMonitorOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.st.Overview(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"overview": ov, "now": time.Now().UTC(), "telemetry": s.tel != nil}
	if s.rec != nil {
		out["stats"] = s.rec.Stats()
	}
	if s.tel != nil {
		out["live"] = s.tel.Snapshot()
	}
	out["loadtest"] = s.loadLive()
	ok(w, out)
}

// handleMonitorSeries: global series plus the series of the chosen cameras on one time grid.
func (s *Server) handleMonitorSeries(w http.ResponseWriter, r *http.Request) {
	if s.tel == nil {
		fail(w, http.StatusServiceUnavailable, "telemetry disabled")
		return
	}
	q := r.URL.Query()
	rng, _ := strconv.Atoi(q.Get("range"))
	if rng <= 0 {
		rng = 300
	}
	if rng > 86400 {
		rng = 86400
	}
	names, top := s.resolveCams(q.Get("cams"))
	g, cams := s.tel.Series(rng, names)
	g.Out, g.In, g.Loop = r1s(g.Out), r1s(g.In), r1s(g.Loop)
	g.NicTx, g.NicRx, g.CPU = r1s(g.NicTx), r1s(g.NicRx), r1s(g.CPU)
	g.APIms, g.RTSPms, g.Loss = r1s(g.APIms), r1s(g.RTSPms), r1s(g.Loss)
	for k, v := range cams {
		v.Out, v.In, v.Loss = r1s(v.Out), r1s(v.In), r1s(v.Loss)
		cams[k] = v
	}

	// load test windows that overlap the range, to draw them on the charts
	type mark struct {
		ID    string `json:"id"`
		Mode  string `json:"mode"`
		Start int64  `json:"start"`
		End   int64  `json:"end"`
	}
	marks := []mark{}
	if len(g.TS) > 0 {
		from := int64(g.TS[0])
		for _, run := range s.runs.list() {
			end := time.Now().Unix()
			if run.FinishedAt != nil {
				end = run.FinishedAt.Unix()
			}
			if end >= from {
				marks = append(marks, mark{ID: run.ID, Mode: run.Mode, Start: run.CreatedAt.Unix(), End: end})
			}
		}
	}
	ok(w, map[string]any{"range": rng, "global": g, "cams": cams, "names": names, "top": top, "marks": marks})
}

// handleMonitorCameras: every camera with live bandwidth, quality and codec.
func (s *Server) handleMonitorCameras(w http.ResponseWriter, r *http.Request) {
	rows, err := s.cameraRows(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	state, group := q.Get("state"), q.Get("group")
	filtered := rows[:0]
	for _, c := range rows {
		if state != "" && c.State != state {
			continue
		}
		if group != "" && c.Group != group {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(c.Slug+" "+c.Name+" "+c.Group), needle) {
			continue
		}
		filtered = append(filtered, c)
	}
	rows = filtered
	switch q.Get("sort") {
	case "name":
		sort.Slice(rows, func(i, j int) bool { return rows[i].Slug < rows[j].Slug })
	case "readers":
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Readers != rows[j].Readers {
				return rows[i].Readers > rows[j].Readers
			}
			return rows[i].Slug < rows[j].Slug
		})
	case "loss":
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].LossTotal != rows[j].LossTotal {
				return rows[i].LossTotal > rows[j].LossTotal
			}
			return rows[i].Slug < rows[j].Slug
		})
	default:
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].OutKbps != rows[j].OutKbps {
				return rows[i].OutKbps > rows[j].OutKbps
			}
			if rows[i].Readers != rows[j].Readers {
				return rows[i].Readers > rows[j].Readers
			}
			return rows[i].Slug < rows[j].Slug
		})
	}
	total := len(rows)
	if lim, _ := strconv.Atoi(q.Get("limit")); lim > 0 && lim < len(rows) {
		rows = rows[:lim]
	}
	for i := range rows {
		rows[i].OutKbps, rows[i].AvgOut, rows[i].PeakOut, rows[i].InKbps = r1(rows[i].OutKbps), r1(rows[i].AvgOut), r1(rows[i].PeakOut), r1(rows[i].InKbps)
		rows[i].PerReader, rows[i].LossPct, rows[i].LossTotal = r1(rows[i].PerReader), r1(rows[i].LossPct), math.Round(rows[i].LossTotal*100)/100
	}
	ok(w, map[string]any{"items": rows, "total": total})
}

// handleMonitorCamera: one camera's live detail, with the readers attached to it.
func (s *Server) handleMonitorCamera(w http.ResponseWriter, r *http.Request) {
	cam, err := s.st.GetCamera(r.Context(), r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "camera not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var paths map[string]telemetry.PathLive
	var sessions []telemetry.SessionView
	if s.tel != nil {
		if p, found := s.tel.Path(cam.Slug); found {
			paths = map[string]telemetry.PathLive{cam.Slug: p}
		}
		sessions = s.tel.Sessions(cam.Slug)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Kbps > sessions[j].Kbps })
	ok(w, map[string]any{"camera": s.buildRow(cam, paths), "sessions": sessions})
}

// handleMonitorStream streams live counters as Server-Sent Events.
//
//	event: stats  every 2 s  (legacy overview, used by the top bar)
//	event: live   every 1 s  (global point + the cameras named in ?cams=, "top:N" allowed)
func (s *Server) handleMonitorStream(w http.ResponseWriter, r *http.Request) {
	flusher, okf := w.(http.Flusher)
	if !okf {
		fail(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	camSpec := r.URL.Query().Get("cams")

	sendStats := func() bool {
		ov, err := s.st.Overview(r.Context())
		if err != nil {
			return true
		}
		st := s.rec.Stats()
		if s.tel != nil {
			l := s.tel.Snapshot()
			st.Readers, st.OutKbps, st.InKbps = l.Readers, int64(l.OutKbps), int64(l.InKbps)
		}
		ov.Readers, ov.InKbps, ov.OutKbps = st.Readers, st.InKbps, st.OutKbps
		payload, _ := json.Marshal(map[string]any{"overview": ov, "stats": st, "now": time.Now().UTC()})
		if _, err := fmt.Fprintf(w, "event: stats\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	sendLive := func() bool {
		if s.tel == nil {
			return true
		}
		l := s.tel.Snapshot()
		names, top := s.resolveCams(camSpec)
		cams := make(map[string]map[string]any, len(names))
		for _, n := range names {
			p, found := s.tel.Path(n)
			if !found {
				cams[n] = map[string]any{"o": 0, "i": 0, "r": 0, "l": 0}
				continue
			}
			cams[n] = map[string]any{"o": r1(p.OutKbps), "i": r1(p.InKbps), "r": p.Readers, "l": r1(p.LossPct), "s": p.Sessions}
		}
		body := map[string]any{
			"ts": time.Now().Unix(),
			"g": map[string]any{
				"out": r1(l.OutKbps), "in": r1(l.InKbps), "loop": r1(l.LoopKbps), "readers": l.Readers, "ready": l.Active,
				"nicTx": r1(l.Host.NicTx), "nicRx": r1(l.Host.NicRx), "cpu": r1(l.Host.CPU), "mem": r1(l.Host.MemPct),
				"api": r1(l.APIms), "rtsp": r1(l.RTSPms), "loss": r1(l.LossPct), "sessions": l.Sessions,
				"online": l.MediaOnline, "peak": r1(l.PeakOut), "peakAt": l.PeakOutAt, "peakReaders": l.PeakReaders,
				"readersBy": l.ReadersBy, "totalOut": l.TotalOut, "totalIn": l.TotalIn,
				"heap": l.Heap, "goroutines": l.Goroutines, "hostOk": l.Host.Available, "load1": r1(l.Host.Load1),
			},
			"c": cams,
		}
		if top {
			body["top"] = names
		}
		if lt := s.loadLive(); lt != nil {
			body["lt"] = lt
		}
		payload, _ := json.Marshal(body)
		if _, err := fmt.Fprintf(w, "event: live\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !sendStats() || !sendLive() {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	n := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			n++
			if !sendLive() {
				return
			}
			if n%2 == 0 && !sendStats() {
				return
			}
		}
	}
}
