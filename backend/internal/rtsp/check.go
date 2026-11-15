// Package rtsp implements a minimal RTSP client used for health checks and
// load testing. It only performs DESCRIBE, which validates connectivity,
// authentication and the SDP offer without pulling any media.
package rtsp

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Result is the outcome of one DESCRIBE.
type Result struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	Server    string `json:"server,omitempty"`
	SDPBytes  int    `json:"sdpBytes,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Describe performs a DESCRIBE on an rtsp:// URL.
func Describe(ctx context.Context, rawURL string, timeout time.Duration) Result {
	return DescribeWithAuth(ctx, rawURL, "", "", timeout)
}

// DescribeWithAuth performs a DESCRIBE using explicit credentials when given.
func DescribeWithAuth(ctx context.Context, rawURL, user, pass string, timeout time.Duration) Result {
	out := Result{}
	u, err := url.Parse(rawURL)
	if err != nil {
		out.Error = "invalid url: " + err.Error()
		return out
	}
	if u.Scheme != "rtsp" && u.Scheme != "rtsps" {
		out.Error = "unsupported scheme " + u.Scheme
		return out
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		if u.Scheme == "rtsps" {
			host = net.JoinHostPort(host, "322")
		} else {
			host = net.JoinHostPort(host, "554")
		}
	}
	if user == "" && u.User != nil {
		user = u.User.Username()
		if p, ok := u.User.Password(); ok {
			pass = p
		}
	}
	requestURL := *u
	requestURL.User = nil
	requestURL.Fragment = ""

	deadline := time.Now().Add(timeout)
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var d net.Dialer
	start := time.Now()
	conn, err := d.DialContext(dctx, "tcp", host)
	if err != nil {
		out.Error = "dial: " + err.Error()
		out.LatencyMS = time.Since(start).Milliseconds()
		return out
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	var sb strings.Builder
	fmt.Fprintf(&sb, "DESCRIBE %s RTSP/1.0\r\n", requestURL.String())
	sb.WriteString("CSeq: 1\r\n")
	sb.WriteString("Accept: application/sdp\r\n")
	sb.WriteString("User-Agent: RestreamCamera/1.0\r\n")
	if user != "" {
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		sb.WriteString("Authorization: Basic " + token + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := conn.Write([]byte(sb.String())); err != nil {
		out.Error = "write: " + err.Error()
		return out
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	out.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		out.Error = "read: " + err.Error()
		return out
	}
	parts := strings.Fields(strings.TrimSpace(statusLine))
	if len(parts) < 2 {
		out.Error = "malformed status line"
		return out
	}
	out.Status, _ = strconv.Atoi(parts[1])

	contentLength := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "content-length":
				contentLength, _ = strconv.Atoi(strings.TrimSpace(v))
			case "server":
				out.Server = strings.TrimSpace(v)
			}
		}
	}
	out.SDPBytes = contentLength
	if out.Status == 200 {
		out.OK = true
		return out
	}
	switch out.Status {
	case 401:
		out.Error = "unauthorized (401)"
	case 404:
		out.Error = "path not found (404)"
	default:
		out.Error = fmt.Sprintf("rtsp status %d", out.Status)
	}
	return out
}

// SplitCredentials extracts userinfo from a URL, returning the URL without it.
func SplitCredentials(raw string) (clean, user, pass string) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, "", ""
	}
	user = u.User.Username()
	pass, _ = u.User.Password()
	u.User = nil
	return u.String(), user, pass
}
