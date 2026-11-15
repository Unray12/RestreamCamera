package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleMonitorStream streams live counters as Server-Sent Events.
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

	send := func() bool {
		ctx := r.Context()
		ov, err := s.st.Overview(ctx)
		if err != nil {
			return true
		}
		st := s.rec.Stats()
		ov.Readers = st.Readers
		ov.InKbps = st.InKbps
		ov.OutKbps = st.OutKbps
		payload, _ := json.Marshal(map[string]any{
			"overview": ov,
			"stats":    st,
			"now":      time.Now().UTC(),
		})
		if _, err := fmt.Fprintf(w, "event: stats\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
