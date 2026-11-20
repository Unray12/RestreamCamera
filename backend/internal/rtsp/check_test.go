package rtsp

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"
)

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

// fakeCamera answers DESCRIBE like an IP camera: 401 + the given challenge, then 200 once the Authorization header verifies.
func fakeCamera(t *testing.T, challenge string, verify func(auth string) bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				auth := ""
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimRight(line, "\r\n")
					if line == "" {
						break
					}
					if strings.HasPrefix(strings.ToLower(line), "authorization:") {
						auth = strings.TrimSpace(line[len("authorization:"):])
					}
				}
				if auth != "" && verify(auth) {
					_, _ = c.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: 2\r\nContent-Length: 0\r\n\r\n"))
					return
				}
				_, _ = c.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: 1\r\nWWW-Authenticate: " + challenge + "\r\n\r\n"))
			}(c)
		}
	}()
	return ln.Addr().String()
}

// digestVerifier recomputes the response the way a camera does (RFC 2617, MD5).
func digestVerifier(user, pass string) func(string) bool {
	return func(auth string) bool {
		if !strings.HasPrefix(auth, "Digest ") {
			return false
		}
		p := parseParams(auth[len("Digest "):])
		ha1 := md5hex(user + ":" + p["realm"] + ":" + pass)
		ha2 := md5hex("DESCRIBE:" + p["uri"])
		want := md5hex(ha1 + ":" + p["nonce"] + ":" + ha2)
		if p["qop"] == "auth" {
			want = md5hex(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
		}
		return p["username"] == user && p["response"] == want
	}
}

func describe(addr, cred string) Result {
	return Describe(context.Background(), "rtsp://"+cred+addr+"/Streaming/Channels/101", 3*time.Second)
}

// The camera that failed in production: Digest only, no qop.
func TestDigestOnlyCameraAccepted(t *testing.T) {
	addr := fakeCamera(t, `Digest realm="Test Camera", nonce="737d3f09", stale="FALSE"`, digestVerifier("admin", "secret"))
	if r := describe(addr, "admin:secret@"); !r.OK || r.Status != 200 {
		t.Fatalf("digest without qop: %+v", r)
	}
}

func TestDigestWithQopAccepted(t *testing.T) {
	addr := fakeCamera(t, `Digest realm="cam", nonce="abc", qop="auth,auth-int", opaque="xyz"`, digestVerifier("admin", "p,w"))
	if r := describe(addr, "admin:p%2Cw@"); !r.OK {
		t.Fatalf("digest with qop and a comma in the password: %+v", r)
	}
}

func TestWrongPasswordIsUnauthorized(t *testing.T) {
	addr := fakeCamera(t, `Digest realm="cam", nonce="abc"`, digestVerifier("admin", "right"))
	r := describe(addr, "admin:wrong@")
	if r.OK || r.Status != 401 || !strings.Contains(r.Error, "401") {
		t.Fatalf("want 401, got %+v", r)
	}
}

func TestBasicCameraStillAccepted(t *testing.T) {
	addr := fakeCamera(t, `Basic realm="cam"`, func(a string) bool { return a == "Basic YWRtaW46cHc=" })
	if r := describe(addr, "admin:pw@"); !r.OK {
		t.Fatalf("basic: %+v", r)
	}
}

func TestNoCredentialsExplainsTheCause(t *testing.T) {
	addr := fakeCamera(t, `Digest realm="cam", nonce="abc"`, digestVerifier("a", "b"))
	if r := describe(addr, ""); r.OK || !strings.Contains(r.Error, "no credentials") {
		t.Fatalf("got %+v", r)
	}
}

func TestUnsupportedDigestAlgorithmIsRefusedNotGuessed(t *testing.T) {
	addr := fakeCamera(t, `Digest realm="cam", nonce="abc", algorithm=SHA-256`, digestVerifier("a", "b"))
	if r := describe(addr, "a:b@"); r.OK || r.Status != 401 {
		t.Fatalf("got %+v", r)
	}
}
