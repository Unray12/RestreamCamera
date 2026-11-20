package rtsp

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeMediaCamera speaks just enough RTSP to serve one H264 track over interleaved TCP.
// It skips one RTP sequence number so the client must report exactly one lost packet.
func fakeMediaCamera(t *testing.T, fps int, runFor time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			method := strings.Fields(line)[0]
			cseq := ""
			for {
				h, err := br.ReadString('\n')
				if err != nil {
					return
				}
				h = strings.TrimRight(h, "\r\n")
				if h == "" {
					break
				}
				if strings.HasPrefix(strings.ToLower(h), "cseq:") {
					cseq = strings.TrimSpace(h[5:])
				}
			}
			switch method {
			case "DESCRIBE":
				sdp := "v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:trackID=0\r\n"
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nServer: fake\r\nContent-Base: rtsp://x/cam/\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", cseq, len(sdp), sdp)
			case "SETUP":
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: abc;timeout=60\r\nTransport: RTP/AVP/TCP;unicast;interleaved=0-1\r\n\r\n", cseq)
			case "PLAY":
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\nSession: abc\r\n\r\n", cseq)
				go func() {
					seq := uint16(100)
					ts := uint32(1000)
					tick := time.NewTicker(time.Second / time.Duration(fps))
					defer tick.Stop()
					end := time.Now().Add(runFor)
					n := 0
					for time.Now().Before(end) {
						<-tick.C
						n++
						if n == 10 {
							seq++ // one packet never sent
						}
						pkt := make([]byte, 12+200)
						pkt[0], pkt[1] = 0x80, 0x80|96
						binary.BigEndian.PutUint16(pkt[2:], seq)
						binary.BigEndian.PutUint32(pkt[4:], ts)
						frame := make([]byte, 4+len(pkt))
						frame[0], frame[1] = '$', 0
						binary.BigEndian.PutUint16(frame[2:], uint16(len(pkt)))
						copy(frame[4:], pkt)
						if _, err := conn.Write(frame); err != nil {
							return
						}
						seq++
						ts += uint32(90000 / fps)
					}
				}()
			case "TEARDOWN":
				return
			default:
				fmt.Fprintf(conn, "RTSP/1.0 200 OK\r\nCSeq: %s\r\n\r\n", cseq)
			}
		}
	}()
	return "rtsp://" + ln.Addr().String() + "/cam"
}

func TestPlayMeasuresMedia(t *testing.T) {
	u := fakeMediaCamera(t, 25, 3*time.Second)
	live := &Live{}
	res := Play(context.Background(), PlayOptions{URL: u, Duration: 1500 * time.Millisecond, Timeout: 3 * time.Second, Live: live})
	if !res.OK {
		t.Fatalf("play failed: %+v", res)
	}
	if res.Codec != "H264" || res.Tracks != 1 {
		t.Fatalf("codec/tracks: %+v", res)
	}
	if res.Frames < 25 || res.Frames > 45 {
		t.Fatalf("frames out of range: %d", res.Frames)
	}
	if res.Lost != 1 {
		t.Fatalf("want exactly 1 lost packet, got %d", res.Lost)
	}
	if res.FPS < 15 || res.FPS > 30 {
		t.Fatalf("fps out of range: %.1f", res.FPS)
	}
	if res.AvgKbps <= 0 || res.TTFBMS < 0 || res.Ended != "completed" {
		t.Fatalf("unexpected: %+v", res)
	}
	if live.Bytes.Load() != res.Bytes || live.Playing.Load() {
		t.Fatalf("live counters not consistent: bytes=%d res=%d playing=%v", live.Bytes.Load(), res.Bytes, live.Playing.Load())
	}
}

func TestPlayCancelKeepsMedia(t *testing.T) {
	u := fakeMediaCamera(t, 25, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res := Play(ctx, PlayOptions{URL: u, Timeout: 3 * time.Second})
	if !res.OK || res.Ended != "cancelled" {
		t.Fatalf("cancelled playback with media should be ok: %+v", res)
	}
}

func TestPlayConnectionRefused(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	res := Play(context.Background(), PlayOptions{URL: "rtsp://" + addr + "/x", Timeout: time.Second})
	if res.OK || res.Phase != "connect" {
		t.Fatalf("want connect failure, got %+v", res)
	}
}

func TestParseSDPAndTrackURL(t *testing.T) {
	m := parseSDP("v=0\r\nm=video 0 RTP/AVP 96\r\na=rtpmap:96 H265/90000\r\na=control:track1\r\nm=audio 0 RTP/AVP 8\r\na=control:rtsp://h/p/audio\r\n")
	if len(m) != 2 || m[0].codec != "H265" || m[0].clock != 90000 || m[1].clock != 8000 {
		t.Fatalf("sdp: %+v", m)
	}
	if got := trackURL("rtsp://h/p", "track1"); got != "rtsp://h/p/track1" {
		t.Fatal(got)
	}
	if got := trackURL("rtsp://h/p/", "rtsp://h/p/audio"); got != "rtsp://h/p/audio" {
		t.Fatal(got)
	}
	if got := trackURL("rtsp://h/p", "*"); got != "rtsp://h/p" {
		t.Fatal(got)
	}
	_ = io.EOF
}
