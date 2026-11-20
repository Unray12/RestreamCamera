package api

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aclab/restreamcamera/internal/model"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

type cameraInput struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	SourceID    string `json:"sourceId"`
	Group       string `json:"group"`
	Count       int    `json:"count"`
	Prefix      string `json:"prefix"`
	StartIndex  int    `json:"startIndex"`
	OnDemand    *bool  `json:"onDemand"`
	MaxReaders  *int   `json:"maxReaders"`
	Mode        string `json:"mode"`
	ReadUser    string `json:"readUser"`
	ReadPass    string `json:"readPass"`
	IPAllowlist string `json:"ipAllowlist"`
	Enabled     *bool  `json:"enabled"`
	Vendor      string `json:"vendor"`
	Model       string `json:"model"`
	Serial      string `json:"serial"`
	Firmware    string `json:"firmware"`
}

func slugify(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	lastDash := false
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '.' || r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Server) handleListCameras(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	cq := model.CameraQuery{
		Q:        strings.TrimSpace(q.Get("q")),
		Group:    q.Get("group"),
		SourceID: q.Get("sourceId"),
		State:    q.Get("state"),
		Limit:    pageSize,
		Offset:   (page - 1) * pageSize,
	}
	switch q.Get("enabled") {
	case "true":
		v := true
		cq.Enabled = &v
	case "false":
		v := false
		cq.Enabled = &v
	}
	items, total, err := s.st.ListCameras(r.Context(), cq)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{
		"items":    items,
		"total":    total,
		"page":     page,
		"pageSize": pageSize,
	})
}

func (s *Server) handleCreateCameras(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in cameraInput
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	src, err := s.st.GetSource(ctx, in.SourceID)
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusBadRequest, "source not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	count := in.Count
	if count <= 0 {
		count = 1
	}
	if count > s.cfg.MaxBulkCameras {
		fail(w, http.StatusBadRequest, fmt.Sprintf("count exceeds limit of %d", s.cfg.MaxBulkCameras))
		return
	}
	onDemand := true
	if in.OnDemand != nil {
		onDemand = *in.OnDemand
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	maxReaders := 4
	if in.MaxReaders != nil {
		maxReaders = *in.MaxReaders
	}
	if maxReaders < 0 {
		maxReaders = 0
	}
	mode := in.Mode
	if mode == "" {
		mode = "proxy"
	}
	if mode != "proxy" && mode != "redirect" {
		fail(w, http.StatusBadRequest, "mode must be proxy or redirect")
		return
	}

	baseName := strings.TrimSpace(in.Name)
	if baseName == "" {
		baseName = "Camera"
	}
	prefix := strings.TrimSpace(in.Prefix)
	if count > 1 && prefix == "" {
		prefix = baseName
	}
	start := in.StartIndex
	if start <= 0 {
		start = 1
	}

	cams := make([]model.Camera, 0, count)
	passwords := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var name, slug string
		if count > 1 {
			name = fmt.Sprintf("%s %04d", prefix, start+i)
			slug = slugify(fmt.Sprintf("%s-%04d", prefix, start+i))
		} else {
			name = baseName
			slug = slugify(in.Slug)
			if slug == "" {
				slug = slugify(baseName)
			}
		}
		if !slugRe.MatchString(slug) {
			fail(w, http.StatusBadRequest, "invalid slug: "+slug)
			return
		}
		pass := in.ReadPass
		if pass == "" {
			pass = randomToken(10)
		}
		enc, err := s.box.Encrypt(pass)
		if err != nil {
			fail(w, http.StatusInternalServerError, "cannot encrypt password")
			return
		}
		readUser := strings.TrimSpace(in.ReadUser)
		if readUser == "" {
			readUser = slug
		}
		cams = append(cams, model.Camera{
			Name:        name,
			Slug:        slug,
			SourceID:    src.ID,
			Group:       strings.TrimSpace(in.Group),
			Enabled:     enabled,
			OnDemand:    onDemand,
			MaxReaders:  maxReaders,
			Mode:        mode,
			ReadUser:    readUser,
			PassEnc:     enc,
			IPAllowlist: strings.TrimSpace(in.IPAllowlist),
			Vendor:      in.Vendor,
			Model:       in.Model,
			Serial:      in.Serial,
			Firmware:    in.Firmware,
		})
		passwords = append(passwords, pass)
	}

	if err := s.st.CreateCameras(ctx, cams); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			fail(w, http.StatusConflict, "duplicate slug, choose another prefix or start index")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]map[string]any, 0, len(cams))
	for i, c := range cams {
		out = append(out, map[string]any{
			"id":       c.ID,
			"name":     c.Name,
			"slug":     c.Slug,
			"readUser": c.ReadUser,
			"readPass": passwords[i],
		})
	}
	_ = s.st.AddEvent(ctx, "info", "camera", src.Name,
		fmt.Sprintf("created %d virtual camera(s)", len(cams)))
	s.rec.Trigger()
	created(w, map[string]any{"created": len(out), "items": out})
}

func (s *Server) handleGetCamera(w http.ResponseWriter, r *http.Request) {
	cam, err := s.st.GetCamera(r.Context(), r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "camera not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"camera": cam, "preview": s.previewURLs(cam)})
}

func (s *Server) handlePatchCamera(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cam, err := s.st.GetCamera(ctx, r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "camera not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	var in cameraInput
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if v := strings.TrimSpace(in.Name); v != "" {
		cam.Name = v
	}
	if in.Group != "" {
		cam.Group = in.Group
	}
	if in.IPAllowlist != "" {
		cam.IPAllowlist = in.IPAllowlist
	}
	if in.Mode != "" {
		if in.Mode != "proxy" && in.Mode != "redirect" {
			fail(w, http.StatusBadRequest, "mode must be proxy or redirect")
			return
		}
		cam.Mode = in.Mode
	}
	if in.Enabled != nil {
		cam.Enabled = *in.Enabled
	}
	if in.OnDemand != nil {
		cam.OnDemand = *in.OnDemand
	}
	if in.MaxReaders != nil && *in.MaxReaders >= 0 {
		cam.MaxReaders = *in.MaxReaders
	}
	if v := strings.TrimSpace(in.ReadUser); v != "" {
		cam.ReadUser = v
	}
	if in.Vendor != "" {
		cam.Vendor = in.Vendor
	}
	if in.Model != "" {
		cam.Model = in.Model
	}
	if in.Serial != "" {
		cam.Serial = in.Serial
	}
	if in.Firmware != "" {
		cam.Firmware = in.Firmware
	}
	if err := s.st.UpdateCamera(ctx, cam); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	newPass := ""
	if in.ReadPass != "" {
		enc, err := s.box.Encrypt(in.ReadPass)
		if err != nil {
			fail(w, http.StatusInternalServerError, "cannot encrypt password")
			return
		}
		if err := s.st.SetCameraPass(ctx, cam.ID, enc); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		newPass = in.ReadPass
	}
	_ = s.st.AddEvent(ctx, "info", "camera", cam.Slug, "camera updated")
	s.rec.Trigger()
	ok(w, map[string]any{"updated": true, "readPass": newPass})
}

func (s *Server) handleDeleteCamera(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cam, err := s.st.GetCamera(ctx, r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "camera not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.mtx.DeletePath(ctx, cam.Slug)
	if err := s.st.DeleteCamera(ctx, cam.ID); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.st.AddEvent(ctx, "info", "camera", cam.Slug, "camera deleted")
	s.rec.Trigger()
	ok(w, map[string]any{"deleted": true})
}

func (s *Server) handleCameraAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cam, err := s.st.GetCamera(ctx, r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "camera not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	action := r.PathValue("action")
	switch action {
	case "enable", "disable":
		enabled := action == "enable"
		if err := s.st.SetCameraEnabled(ctx, cam.ID, enabled); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !enabled {
			_ = s.mtx.DeletePath(ctx, cam.Slug)
		}
		_ = s.st.AddEvent(ctx, "info", "camera", cam.Slug, "camera "+action+"d")
		s.rec.Trigger()
		ok(w, map[string]any{"ok": true})
	case "restart":
		_ = s.mtx.DeletePath(ctx, cam.Slug)
		s.rec.Trigger()
		ok(w, map[string]any{"ok": true})
	case "rotate-password":
		pass := randomToken(10)
		enc, err := s.box.Encrypt(pass)
		if err != nil {
			fail(w, http.StatusInternalServerError, "cannot encrypt password")
			return
		}
		if err := s.st.SetCameraPass(ctx, cam.ID, enc); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		_ = s.st.AddEvent(ctx, "warn", "camera", cam.Slug, "credential rotated")
		ok(w, map[string]any{"ok": true, "readPass": pass})
	default:
		fail(w, http.StatusBadRequest, "unknown action")
	}
}

func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(in.IDs) == 0 {
		fail(w, http.StatusBadRequest, "ids is required")
		return
	}
	switch in.Action {
	case "enable", "disable":
		n, err := s.st.SetCamerasEnabled(ctx, in.IDs, in.Action == "enable")
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if in.Action == "disable" {
			for _, id := range in.IDs {
				if cam, err := s.st.GetCamera(ctx, id); err == nil {
					_ = s.mtx.DeletePath(ctx, cam.Slug)
				}
			}
		}
		_ = s.st.AddEvent(ctx, "info", "camera", "", fmt.Sprintf("%sd %d camera(s)", in.Action, n))
		s.rec.Trigger()
		ok(w, map[string]any{"affected": n})
	case "delete":
		deleted := 0
		for _, id := range in.IDs {
			cam, err := s.st.GetCamera(ctx, id)
			if err != nil {
				continue
			}
			_ = s.mtx.DeletePath(ctx, cam.Slug)
		}
		n, err := s.st.DeleteCameras(ctx, in.IDs)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		deleted += n
		_ = s.st.AddEvent(ctx, "warn", "camera", "", fmt.Sprintf("deleted %d camera(s)", deleted))
		s.rec.Trigger()
		ok(w, map[string]any{"affected": deleted})
	case "rotate-password":
		type rotated struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Pass string `json:"readPass"`
		}
		out := make([]rotated, 0, len(in.IDs))
		for _, id := range in.IDs {
			cam, err := s.st.GetCamera(ctx, id)
			if err != nil {
				continue
			}
			pass := randomToken(10)
			enc, err := s.box.Encrypt(pass)
			if err != nil {
				continue
			}
			if err := s.st.SetCameraPass(ctx, cam.ID, enc); err != nil {
				continue
			}
			out = append(out, rotated{ID: cam.ID, Slug: cam.Slug, Pass: pass})
		}
		_ = s.st.AddEvent(ctx, "warn", "camera", "", fmt.Sprintf("rotated %d credential(s)", len(out)))
		ok(w, map[string]any{"items": out})
	default:
		fail(w, http.StatusBadRequest, "unknown action")
	}
}

func (s *Server) handleCameraPreview(w http.ResponseWriter, r *http.Request) {
	cam, err := s.st.GetCamera(r.Context(), r.PathValue("id"))
	if err != nil {
		if isNotFound(err) {
			fail(w, http.StatusNotFound, "camera not found")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, s.previewURLs(cam))
}

func (s *Server) previewURLs(cam model.Camera) map[string]any {
	user := cam.ReadUser
	pass, _ := s.box.Decrypt(cam.PassEnc)
	auth := ""
	if user != "" && pass != "" {
		auth = url.QueryEscape(user) + ":" + url.QueryEscape(pass) + "@"
	}
	rtsp := "rtsp://" + auth + s.cfg.PublicRTSP + "/" + cam.Slug
	return map[string]any{
		"rtsp":     rtsp,
		"hls":      s.cfg.PublicHLS + "/" + cam.Slug + "/index.m3u8",
		"whep":     s.cfg.PublicWebRTC + "/" + cam.Slug,
		"internal": s.cfg.MediaRTSP + "/" + cam.Slug,
		"readUser": user,
		"readPass": pass,
	}
}

func (s *Server) handleExportCameras(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.allCameras(ctx)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=restream-cameras.csv")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"name", "slug", "group", "source", "rtsp", "hls", "whep", "username", "password"})
	for _, cam := range all {
		p := s.previewURLs(cam)
		_ = cw.Write([]string{
			cam.Name, cam.Slug, cam.Group, cam.SourceName,
			fmt.Sprint(p["rtsp"]), fmt.Sprint(p["hls"]), fmt.Sprint(p["whep"]),
			cam.ReadUser, fmt.Sprint(p["readPass"]),
		})
	}
	cw.Flush()
}

func (s *Server) allCameras(ctx context.Context) ([]model.Camera, error) {
	out := make([]model.Camera, 0, 256)
	for page := 0; ; page++ {
		batch, total, err := s.st.ListCameras(ctx, model.CameraQuery{Limit: 200, Offset: page * 200})
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(out) >= total || len(batch) == 0 {
			return out, nil
		}
	}
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.st.ListSamples(r.Context(), n)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"items": items})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.st.ListEvents(r.Context(), n)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"items": items})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.st.Overview(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := s.rec.Stats()
	if s.tel != nil { // real-time numbers (1 s) beat the 5 s reconcile estimate
		l := s.tel.Snapshot()
		st.Readers, st.OutKbps, st.InKbps = l.Readers, int64(l.OutKbps), int64(l.InKbps)
	}
	ov.Readers = st.Readers
	ov.InKbps = st.InKbps
	ov.OutKbps = st.OutKbps
	ok(w, map[string]any{"overview": ov, "stats": st, "now": time.Now().UTC()})
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	info, mtxErr := s.mtx.Info(ctx)
	out := map[string]any{
		"version":      s.cfg.Version,
		"uptimeSec":    int(time.Since(s.started).Seconds()),
		"startedAt":    s.started.UTC(),
		"stats":        s.rec.Stats(),
		"mediaApi":     s.cfg.MediaAPI,
		"mediaRtsp":    s.cfg.MediaRTSP,
		"publicRtsp":   s.cfg.PublicRTSP,
		"publicHls":    s.cfg.PublicHLS,
		"publicWebrtc": s.cfg.PublicWebRTC,
		"prefixes": map[string]string{
			"cameras": s.cfg.CamerasPrefix,
			"sources": s.cfg.SourcesPrefix,
		},
	}
	if mtxErr == nil {
		out["media"] = info
		out["mediaOnline"] = true
	} else {
		out["media"] = map[string]any{"error": mtxErr.Error()}
		out["mediaOnline"] = false
	}
	ok(w, out)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ov, err := s.st.Overview(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := s.rec.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP restreamcamera_cameras_total Number of virtual cameras.\n")
	fmt.Fprintf(w, "# TYPE restreamcamera_cameras_total gauge\n")
	fmt.Fprintf(w, "restreamcamera_cameras_total %d\n", ov.CamerasTotal)
	fmt.Fprintf(w, "restreamcamera_cameras_enabled %d\n", ov.CamerasEnabled)
	fmt.Fprintf(w, "restreamcamera_cameras_ready %d\n", ov.CamerasReady)
	fmt.Fprintf(w, "restreamcamera_sources_total %d\n", ov.SourcesTotal)
	fmt.Fprintf(w, "restreamcamera_readers %d\n", st.Readers)
	fmt.Fprintf(w, "restreamcamera_ingest_kbps %d\n", st.InKbps)
	fmt.Fprintf(w, "restreamcamera_egress_kbps %d\n", st.OutKbps)
	fmt.Fprintf(w, "restreamcamera_managed_paths %d\n", st.ManagedPaths)
}
