package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aclab/restreamcamera/internal/model"
	"github.com/aclab/restreamcamera/internal/rtsp"
)

var pullSchemes = map[string]bool{
	"rtsp": true, "rtsps": true, "rtmp": true, "rtmps": true,
	"http": true, "https": true, "srt": true, "udp": true, "udp+mpegts": true,
}

type sourceInput struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Transport string `json:"transport"`
	Enabled   *bool  `json:"enabled"`
}

func (in *sourceInput) validate(creating bool) string {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return "name is required"
	}
	if in.Kind == "" {
		in.Kind = "pull"
	}
	if in.Kind != "pull" && in.Kind != "publisher" {
		return "kind must be pull or publisher"
	}
	if in.Transport == "" {
		in.Transport = "tcp"
	}
	if in.Transport != "tcp" && in.Transport != "udp" {
		return "transport must be tcp or udp"
	}
	if in.Kind == "pull" {
		if strings.TrimSpace(in.URL) == "" {
			return "url is required for a pull source"
		}
		u, err := url.Parse(in.URL)
		if err != nil || u.Host == "" {
			return "url is not valid"
		}
		if !pullSchemes[u.Scheme] {
			return "unsupported url scheme " + u.Scheme
		}
	} else if creating {
		in.URL = ""
	}
	return ""
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.ListSources(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"items": items, "total": len(items)})
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	var in sourceInput
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if msg := in.validate(true); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	enc, err := s.box.Encrypt(in.URL)
	if err != nil {
		fail(w, http.StatusInternalServerError, "cannot encrypt url")
		return
	}
	src, err := s.st.CreateSource(r.Context(), model.Source{
		Name:      in.Name,
		Kind:      in.Kind,
		Transport: in.Transport,
		Enabled:   enabled,
		URL:       in.URL,
	}, enc)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.st.AddEvent(r.Context(), "info", "source", src.Name, "source created")
	s.rec.Trigger()
	created(w, src)
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.st.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "source not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	count, _ := s.st.CountCamerasBySource(r.Context(), src.ID)
	ok(w, map[string]any{"source": src, "cameraCount": count})
}

func (s *Server) handlePatchSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, err := s.st.GetSource(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "source not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var in sourceInput
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	// merge: empty fields keep current values
	if in.Name == "" {
		in.Name = cur.Name
	}
	if in.Kind == "" {
		in.Kind = cur.Kind
	}
	if in.Transport == "" {
		in.Transport = cur.Transport
	}
	if in.Enabled == nil {
		in.Enabled = &cur.Enabled
	}
	if msg := in.validate(false); msg != "" {
		fail(w, http.StatusBadRequest, msg)
		return
	}
	var encPtr *string
	if in.URL != "" {
		enc, err := s.box.Encrypt(in.URL)
		if err != nil {
			fail(w, http.StatusInternalServerError, "cannot encrypt url")
			return
		}
		encPtr = &enc
	}
	err = s.st.UpdateSource(r.Context(), id, model.Source{
		Name:      in.Name,
		Kind:      in.Kind,
		Transport: in.Transport,
		Enabled:   *in.Enabled,
	}, encPtr)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.st.AddEvent(r.Context(), "info", "source", in.Name, "source updated")
	s.rec.Trigger()
	ok(w, map[string]any{"updated": true})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	src, err := s.st.GetSource(ctx, id)
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "source not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// remove camera paths first so MediaMTX forgets them immediately
	if cams, err := s.st.AllCamerasForReconcile(ctx); err == nil {
		for _, c := range cams {
			if c.SourceID == id {
				_ = s.mtx.DeletePath(ctx, c.Slug)
			}
		}
	}
	if err := s.st.DeleteSource(ctx, id); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.st.AddEvent(ctx, "warn", "source", src.Name, "source deleted with its cameras")
	s.rec.Trigger()
	ok(w, map[string]any{"deleted": true})
}

func (s *Server) handleTestSource(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	src, err := s.st.GetSource(ctx, r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "source not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if src.Kind == "publisher" {
		ok(w, map[string]any{"ok": true, "detail": "publisher path — push a stream to " + src.Path})
		return
	}
	raw, err := s.box.Decrypt(src.URLEnc)
	if err != nil {
		fail(w, http.StatusInternalServerError, "cannot decrypt url")
		return
	}
	u, _ := url.Parse(raw)
	switch u.Scheme {
	case "rtsp", "rtsps":
		res := rtsp.Describe(ctx, raw, 6*time.Second)
		if res.OK {
			_ = s.st.UpdateSourceStatus(ctx, src.ID, "online", "")
		} else {
			_ = s.st.UpdateSourceStatus(ctx, src.ID, "offline", res.Error)
		}
		ok(w, res)
	case "http", "https":
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
		res, err := http.DefaultClient.Do(req)
		lat := time.Since(start).Milliseconds()
		if err != nil {
			ok(w, map[string]any{"ok": false, "latencyMs": lat, "error": err.Error()})
			return
		}
		defer res.Body.Close()
		ok(w, map[string]any{"ok": res.StatusCode < 400, "status": res.StatusCode, "latencyMs": lat})
	default:
		ok(w, map[string]any{"ok": true, "detail": "scheme " + u.Scheme + " cannot be probed"})
	}
}
