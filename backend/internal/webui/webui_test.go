package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestIndexIsServedWithoutRedirect(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/", "/cameras"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<!doctype html>") {
			t.Fatalf("%s: body is not the SPA shell", path)
		}
	}
}

func TestStaticAssets(t *testing.T) {
	h := Handler()
	for _, path := range []string{"/app.js", "/styles.css"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty body", path)
		}
	}
}
