package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type ctxKey string

const ctxUser ctxKey = "user"

func userFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxUser).(string); ok {
		return v
	}
	return ""
}

// signToken creates a compact HMAC-signed token.
func (s *Server) signToken(user string, ttl time.Duration) string {
	payload, _ := json.Marshal(map[string]any{
		"sub": user,
		"exp": time.Now().Add(ttl).Unix(),
	})
	mac := hmac.New(sha256.New, s.cfg.MasterKey)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Server) verifyToken(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, s.cfg.MasterKey)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	if claims.Exp < time.Now().Unix() {
		return "", false
	}
	return claims.Sub, true
}

func (s *Server) auth(next http.Handler) http.Handler {
	public := map[string]bool{
		"/api/v1/auth/login":    true,
		"/api/v1/mediamtx/auth": true,
		"/api/v1/healthz":       true,
		"/healthz":              true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || public[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		user, valid := s.verifyToken(token)
		if !valid {
			fail(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, user)))
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(in.Username), []byte(s.cfg.AdminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(in.Password), []byte(s.cfg.AdminPassword)) == 1
	if !userOK || !passOK {
		time.Sleep(150 * time.Millisecond) // slow down brute force
		fail(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	ok(w, map[string]any{
		"token":     s.signToken(in.Username, 24*time.Hour),
		"username":  in.Username,
		"expiresIn": 86400,
	})
}

// ---------------------------------------------------------- mediamtx auth

type mediaAuthRequest struct {
	User      string `json:"user"`
	Password  string `json:"password"`
	Token     string `json:"token"`
	IP        string `json:"ip"`
	Action    string `json:"action"`
	Path      string `json:"path"`
	Protocol  string `json:"protocol"`
	ID        string `json:"id"`
	Query     string `json:"query"`
	UserAgent string `json:"userAgent"`
}

// handleMediaAuth is called by MediaMTX for every read/publish.
func (s *Server) handleMediaAuth(w http.ResponseWriter, r *http.Request) {
	var in mediaAuthRequest
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, "invalid json")
		return
	}
	ctx := r.Context()

	loopback := isLoopback(in.IP)

	switch in.Action {
	case "read":
		// Internal pulls from a virtual camera to its origin path.
		if strings.HasPrefix(in.Path, s.cfg.SourcesPrefix) {
			if loopback {
				ok(w, map[string]any{})
				return
			}
			// redirect mode: the client is handed the origin path directly.
			if s.originExposed(ctx, in.Path) {
				ok(w, map[string]any{})
				return
			}
			fail(w, http.StatusForbidden, "origin paths are internal")
			return
		}
		cam, err := s.st.GetCameraBySlug(ctx, in.Path)
		if err != nil {
			if isNotFound(err) {
				fail(w, http.StatusUnauthorized, "unknown camera")
				return
			}
			fail(w, http.StatusInternalServerError, "store error")
			return
		}
		if !cam.Enabled {
			fail(w, http.StatusForbidden, "camera disabled")
			return
		}
		if !ipAllowed(cam.IPAllowlist, in.IP) {
			fail(w, http.StatusForbidden, "ip not allowed")
			return
		}
		if cam.PassEnc != "" {
			pass, err := s.box.Decrypt(cam.PassEnc)
			if err != nil {
				fail(w, http.StatusInternalServerError, "credential error")
				return
			}
			userOK := subtle.ConstantTimeCompare([]byte(in.User), []byte(cam.ReadUser)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(in.Password), []byte(pass)) == 1
			if !userOK || !passOK || in.User == "" {
				fail(w, http.StatusUnauthorized, "invalid camera credentials")
				return
			}
		}
		ok(w, map[string]any{})

	case "publish":
		if !strings.HasPrefix(in.Path, s.cfg.SourcesPrefix) {
			fail(w, http.StatusForbidden, "publish is not allowed on this path")
			return
		}
		srcs, err := s.st.ListSources(ctx)
		if err != nil {
			fail(w, http.StatusInternalServerError, "store error")
			return
		}
		for _, src := range srcs {
			if src.Path == in.Path && src.Kind == "publisher" && src.Enabled {
				ok(w, map[string]any{})
				return
			}
		}
		fail(w, http.StatusForbidden, "unknown publisher path")

	default:
		fail(w, http.StatusForbidden, "action not allowed")
	}
}

// originExposed reports whether the origin path may be read from outside the
// host because a camera redirects clients to it.
func (s *Server) originExposed(ctx context.Context, path string) bool {
	srcs, err := s.st.ListSources(ctx)
	if err != nil {
		return false
	}
	for _, src := range srcs {
		if src.Path != path || !src.Enabled {
			continue
		}
		exposed, err := s.st.SourceExposesOrigin(ctx, src.ID)
		return err == nil && exposed
	}
	return false
}

func isLoopback(raw string) bool {
	if raw == "" {
		return false
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func ipAllowed(allowlist, raw string) bool {
	allowlist = strings.TrimSpace(allowlist)
	if allowlist == "" {
		return true
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, entry := range strings.Split(allowlist, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, netw, err := net.ParseCIDR(entry); err == nil && netw.Contains(ip) {
				return true
			}
			continue
		}
		if cand := net.ParseIP(entry); cand != nil && cand.Equal(ip) {
			return true
		}
	}
	return false
}

// helpers shared with other handlers

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("pw%d", time.Now().UnixNano())
	}
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}
