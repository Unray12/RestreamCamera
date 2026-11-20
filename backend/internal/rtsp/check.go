// Package rtsp implements a minimal RTSP client used for health checks and
// load testing. It only performs DESCRIBE, which validates connectivity,
// authentication and the SDP offer without pulling any media.
package rtsp

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
// It first asks without credentials and answers the camera's own challenge
// (Digest or Basic) on a 401, as a real player does: most IP cameras accept
// Digest only, so sending Basic up front always ended in "unauthorized".
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
	uri := requestURL.String()

	deadline := time.Now().Add(timeout)
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	start := time.Now()

	resp, err := describeFlow(dctx, host, uri, user, pass, deadline)
	out.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Status, out.Server, out.SDPBytes = resp.status, resp.server, resp.contentLength
	if out.Status == 200 {
		out.OK = true
		return out
	}
	switch out.Status {
	case 401:
		out.Error = "unauthorized (401)"
		if user == "" {
			out.Error = "unauthorized (401): the url has no credentials"
		}
	case 404:
		out.Error = "path not found (404)"
	default:
		out.Error = fmt.Sprintf("rtsp status %d", out.Status)
	}
	return out
}

type describeReply struct {
	status        int
	server        string
	contentLength int
	challenges    []string // every WWW-Authenticate header value
}

// session is one RTSP TCP connection. Cameras bind a Digest nonce to the connection that received it
// (checked on a Hikvision-style camera: the answer sent on a NEW connection gets 401), so the
// authenticated DESCRIBE must follow the challenge on the SAME connection.
type session struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialSession(ctx context.Context, host string, deadline time.Time) (*session, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, errors.New("dial: " + err.Error())
	}
	_ = conn.SetDeadline(deadline)
	return &session{conn: conn, br: bufio.NewReader(conn)}, nil
}

// describeFlow asks without credentials, then answers a 401 challenge on the same connection.
// When the camera closed the connection after the 401, it answers on a fresh one (nonce not bound).
func describeFlow(ctx context.Context, host, uri, user, pass string, deadline time.Time) (describeReply, error) {
	ss, err := dialSession(ctx, host, deadline)
	if err != nil {
		return describeReply{}, err
	}
	defer func() { ss.conn.Close() }()
	rep, err := ss.describe(uri, "", 1)
	if err != nil || rep.status != 401 || user == "" {
		return rep, err
	}
	auth := authorization(rep.challenges, user, pass, uri)
	if auth == "" {
		return rep, nil
	}
	if next, err := ss.describe(uri, auth, 2); err == nil {
		return next, nil
	}
	ss.conn.Close()
	if ss, err = dialSession(ctx, host, deadline); err != nil {
		return rep, err
	}
	return ss.describe(uri, auth, 2)
}

// describe sends one DESCRIBE and reads the status line and headers (the body is skipped).
func (ss *session) describe(uri, auth string, cseq int) (describeReply, error) {
	var rep describeReply
	var sb strings.Builder
	fmt.Fprintf(&sb, "DESCRIBE %s RTSP/1.0\r\n", uri)
	fmt.Fprintf(&sb, "CSeq: %d\r\n", cseq)
	sb.WriteString("Accept: application/sdp\r\n")
	sb.WriteString("User-Agent: RestreamCamera/1.0\r\n")
	if auth != "" {
		sb.WriteString("Authorization: " + auth + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := ss.conn.Write([]byte(sb.String())); err != nil {
		return rep, errors.New("write: " + err.Error())
	}

	statusLine, err := ss.br.ReadString('\n')
	if err != nil {
		return rep, errors.New("read: " + err.Error())
	}
	parts := strings.Fields(strings.TrimSpace(statusLine))
	if len(parts) < 2 {
		return rep, errors.New("malformed status line")
	}
	rep.status, _ = strconv.Atoi(parts[1])
	for {
		line, err := ss.br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			v = strings.TrimSpace(v)
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "content-length":
				rep.contentLength, _ = strconv.Atoi(v)
			case "server":
				rep.server = v
			case "www-authenticate":
				rep.challenges = append(rep.challenges, v)
			}
		}
	}
	if rep.status != 200 && rep.contentLength > 0 && rep.contentLength <= 64<<10 {
		// drain the error body so the next request on this connection starts at a clean boundary
		_, _ = io.CopyN(io.Discard, ss.br, int64(rep.contentLength))
	}
	return rep, nil
}

// authorization answers the first usable challenge: Digest (preferred) or Basic.
func authorization(challenges []string, user, pass, uri string) string {
	for _, c := range challenges {
		if strings.HasPrefix(strings.ToLower(c), "digest ") {
			if h := digestHeader(c[len("digest "):], user, pass, uri); h != "" {
				return h
			}
		}
	}
	for _, c := range challenges {
		if strings.HasPrefix(strings.ToLower(c), "basic") {
			return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
		}
	}
	return ""
}

// digestHeader builds an RFC 2617 MD5 Digest answer (with or without qop=auth); "" when unsupported.
func digestHeader(params, user, pass, uri string) string {
	p := parseParams(params)
	if alg := strings.ToUpper(p["algorithm"]); alg != "" && alg != "MD5" {
		return ""
	}
	realm, nonce := p["realm"], p["nonce"]
	if nonce == "" {
		return ""
	}
	md5hex := func(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex("DESCRIBE:" + uri)
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s"`, user, realm, nonce, uri)
	qop := ""
	for _, q := range strings.Split(p["qop"], ",") {
		if strings.TrimSpace(q) == "auth" {
			qop = "auth"
		}
	}
	if qop != "" {
		raw := make([]byte, 8)
		_, _ = rand.Read(raw)
		cnonce, nc := hex.EncodeToString(raw), "00000001"
		fmt.Fprintf(&b, `, response="%s", qop=auth, nc=%s, cnonce="%s"`, md5hex(ha1+":"+nonce+":"+nc+":"+cnonce+":auth:"+ha2), nc, cnonce)
	} else {
		fmt.Fprintf(&b, `, response="%s"`, md5hex(ha1+":"+nonce+":"+ha2))
	}
	if o := p["opaque"]; o != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, o)
	}
	return b.String()
}

// parseParams reads `k="v", k2=v2` pairs (values may be quoted and contain commas).
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val, s = s[1:], ""
			} else {
				val, s = s[1:1+end], s[end+2:]
			}
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				end = len(s)
			}
			val, s = strings.TrimSpace(s[:end]), s[end:]
		}
		out[key] = val
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
