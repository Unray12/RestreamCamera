package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/rtsp"
)

type loadRun struct {
	ID          string         `json:"id"`
	Status      string         `json:"status"` // running | done
	CreatedAt   time.Time      `json:"createdAt"`
	FinishedAt  *time.Time     `json:"finishedAt,omitempty"`
	Count       int            `json:"count"`
	Concurrency int            `json:"concurrency"`
	TimeoutMS   int            `json:"timeoutMs"`
	OK          int            `json:"ok"`
	Fail        int            `json:"fail"`
	AvgMS       int64          `json:"avgMs"`
	P50MS       int64          `json:"p50Ms"`
	P95MS       int64          `json:"p95Ms"`
	MaxMS       int64          `json:"maxMs"`
	Errors      map[string]int `json:"errors"`
	Samples     []string       `json:"samples"`
	DurationMS  int64          `json:"durationMs"`
}

type runStore struct {
	mu   sync.Mutex
	runs map[string]*loadRun
}

func newRunStore() *runStore { return &runStore{runs: map[string]*loadRun{}} }

func (rs *runStore) put(r *loadRun) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.runs[r.ID] = r
}

func (rs *runStore) get(id string) (loadRun, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.runs[id]
	if !ok {
		return loadRun{}, false
	}
	cp := *r
	cp.Errors = make(map[string]int, len(r.Errors))
	for k, v := range r.Errors {
		cp.Errors[k] = v
	}
	cp.Samples = append([]string(nil), r.Samples...)
	return cp, true
}

func (s *Server) handleStartLoadtest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Count       int      `json:"count"`
		Concurrency int      `json:"concurrency"`
		TimeoutMS   int      `json:"timeoutMs"`
		CameraIDs   []string `json:"cameraIds"`
		SourceID    string   `json:"sourceId"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
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

	cams, err := s.allCameras(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	selected := make([]model.Camera, 0, len(cams))
	want := map[string]bool{}
	for _, id := range in.CameraIDs {
		want[id] = true
	}
	for _, c := range cams {
		if !c.Enabled {
			continue
		}
		if in.SourceID != "" && c.SourceID != in.SourceID {
			continue
		}
		if len(want) > 0 && !want[c.ID] {
			continue
		}
		selected = append(selected, c)
	}
	if len(selected) == 0 {
		fail(w, http.StatusBadRequest, "no enabled camera matches the selection")
		return
	}

	run := &loadRun{
		ID:          fmt.Sprintf("lt-%d", time.Now().UnixNano()/1e6),
		Status:      "running",
		CreatedAt:   time.Now().UTC(),
		Count:       in.Count,
		Concurrency: in.Concurrency,
		TimeoutMS:   in.TimeoutMS,
		Errors:      map[string]int{},
	}
	s.runs.put(run)

	go s.runLoadtest(run, selected)
	created(w, run)
}

func (s *Server) runLoadtest(run *loadRun, cams []model.Camera) {
	start := time.Now()
	timeout := time.Duration(run.TimeoutMS) * time.Millisecond

	type result struct {
		lat  int64
		ok   bool
		errs string
	}
	jobs := make(chan int)
	results := make(chan result, run.Count)
	var wg sync.WaitGroup

	for w := 0; w < run.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				cam := cams[i%len(cams)]
				pass, _ := s.box.Decrypt(cam.PassEnc)
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				res := rtsp.DescribeWithAuth(ctx, s.cfg.MediaRTSP+"/"+cam.Slug, cam.ReadUser, pass, timeout)
				cancel()
				msg := res.Error
				if msg == "" {
					msg = fmt.Sprintf("status %d", res.Status)
				}
				results <- result{lat: res.LatencyMS, ok: res.OK, errs: msg}
			}
		}()
	}
	go func() {
		for i := 0; i < run.Count; i++ {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	lats := make([]int64, 0, run.Count)
	okCount, failCount := 0, 0
	errs := map[string]int{}
	samples := make([]string, 0, 10)
	for res := range results {
		lats = append(lats, res.lat)
		if res.ok {
			okCount++
			continue
		}
		failCount++
		errs[res.errs]++
		if len(samples) < 10 {
			samples = append(samples, res.errs)
		}
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	var avg int64
	for _, v := range lats {
		avg += v
	}
	if len(lats) > 0 {
		avg /= int64(len(lats))
	}
	pct := func(p float64) int64 {
		if len(lats) == 0 {
			return 0
		}
		i := int(float64(len(lats)-1) * p)
		return lats[i]
	}

	fin := time.Now().UTC()
	s.runs.mu.Lock()
	run.Status = "done"
	run.FinishedAt = &fin
	run.OK, run.Fail = okCount, failCount
	run.AvgMS, run.P50MS, run.P95MS = avg, pct(0.50), pct(0.95)
	if len(lats) > 0 {
		run.MaxMS = lats[len(lats)-1]
	}
	run.Errors = errs
	run.Samples = samples
	run.DurationMS = time.Since(start).Milliseconds()
	s.runs.mu.Unlock()
}

func (s *Server) handleGetLoadtest(w http.ResponseWriter, r *http.Request) {
	run, found := s.runs.get(r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, "run not found")
		return
	}
	ok(w, run)
}
