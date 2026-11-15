// Package mediamtx is a small client for the MediaMTX Control API (v3).
package mediamtx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one MediaMTX instance.
type Client struct {
	base string
	hc   *http.Client
}

// New builds a client for base, e.g. http://127.0.0.1:9997.
func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		hc:   &http.Client{Timeout: 10 * time.Second},
	}
}

// PathConf is the subset of the path configuration we manage.
type PathConf struct {
	Name           string `json:"name"`
	Source         string `json:"source"`
	SourceOnDemand bool   `json:"sourceOnDemand"`
	SourceRedirect string `json:"sourceRedirect,omitempty"`
	MaxReaders     int    `json:"maxReaders"`
	RTSPTransport  string `json:"rtspTransport,omitempty"`
}

// Reader is one path reader (client).
type Reader struct {
	Type string `json:"type"`
}

// PathState is the subset of the runtime path state we consume.
type PathState struct {
	Name          string   `json:"name"`
	Ready         bool     `json:"ready"`
	Available     bool     `json:"available"`
	Readers       []Reader `json:"readers"`
	InboundBytes  uint64   `json:"inboundBytes"`
	OutboundBytes uint64   `json:"outboundBytes"`
}

// Info is the server information payload.
type Info struct {
	Version string    `json:"version"`
	Started time.Time `json:"started"`
}

type listWrapper[T any] struct {
	ItemCount int `json:"itemCount"`
	PageCount int `json:"pageCount"`
	Items     []T `json:"items"`
}

// Error is an API error with the HTTP status code.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("mediamtx: %d %s", e.Status, e.Msg) }

// IsNotFound reports whether the error is a 404.
func IsNotFound(err error) bool {
	var e *Error
	if ok := asError(err, &e); ok {
		return e.Status == http.StatusNotFound
	}
	return false
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mediamtx %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = res.Status
		}
		return &Error{Status: res.StatusCode, Msg: msg}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("mediamtx decode %s: %w", path, err)
		}
	}
	return nil
}

// Info returns server version information.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.do(ctx, http.MethodGet, "/v3/info", nil, &out)
	return out, err
}

// AddPath creates a path configuration.
func (c *Client) AddPath(ctx context.Context, name string, conf PathConf) error {
	conf.Name = ""
	return c.do(ctx, http.MethodPost, "/v3/config/paths/add/"+url.PathEscape(name), conf, nil)
}

// PatchPath updates a path configuration.
func (c *Client) PatchPath(ctx context.Context, name string, conf PathConf) error {
	conf.Name = ""
	return c.do(ctx, http.MethodPatch, "/v3/config/paths/patch/"+url.PathEscape(name), conf, nil)
}

// DeletePath removes a path configuration.
func (c *Client) DeletePath(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/v3/config/paths/delete/"+url.PathEscape(name), nil, nil)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// ListConfigPaths returns every configured path.
func (c *Client) ListConfigPaths(ctx context.Context) ([]PathConf, error) {
	out := make([]PathConf, 0, 256)
	page := 0
	for {
		var pageOut listWrapper[PathConf]
		p := fmt.Sprintf("/v3/config/paths/list?page=%d&itemsPerPage=1000", page)
		if err := c.do(ctx, http.MethodGet, p, nil, &pageOut); err != nil {
			return nil, err
		}
		out = append(out, pageOut.Items...)
		page++
		if page >= pageOut.PageCount || pageOut.PageCount == 0 {
			return out, nil
		}
	}
}

// ListPaths returns runtime state of every active path.
func (c *Client) ListPaths(ctx context.Context) ([]PathState, error) {
	out := make([]PathState, 0, 256)
	page := 0
	for {
		var pageOut listWrapper[PathState]
		p := fmt.Sprintf("/v3/paths/list?page=%d&itemsPerPage=1000", page)
		if err := c.do(ctx, http.MethodGet, p, nil, &pageOut); err != nil {
			return nil, err
		}
		out = append(out, pageOut.Items...)
		page++
		if page >= pageOut.PageCount || pageOut.PageCount == 0 {
			return out, nil
		}
	}
}

// GetPath returns the runtime state of one path.
func (c *Client) GetPath(ctx context.Context, name string) (PathState, error) {
	var out PathState
	err := c.do(ctx, http.MethodGet, "/v3/paths/get/"+url.PathEscape(name), nil, &out)
	return out, err
}
