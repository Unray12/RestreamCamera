package rtsp

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Live holds counters a caller can read while Play is running (all atomic).
// Load tests sum them over every client once a second to draw throughput.
type Live struct {
	Bytes   atomic.Uint64 // wire bytes received on the interleaved channels
	Packets atomic.Uint64 // RTP packets received
	Frames  atomic.Uint64 // video frames (RTP marker bit)
	Lost    atomic.Uint64 // RTP packets missing from the sequence
	Stalls  atomic.Uint64 // gaps of more than StallGap between two packets
	Playing atomic.Bool   // true from the PLAY response until the client ends

	// Milliseconds plus one (0 = not reached yet), set as each milestone happens.
	Handshake atomic.Int64 // dial -> PLAY response
	TTFB      atomic.Int64 // PLAY request -> first RTP packet
	Start     atomic.Int64 // dial -> first RTP packet
}

// PlayOptions configures one full-media RTSP client.
type PlayOptions struct {
	URL      string
	User     string
	Pass     string
	Duration time.Duration // how long to receive media after PLAY; 0 = until ctx is cancelled
	Timeout  time.Duration // per-step handshake timeout and the "no data" stall timeout (default 10s)
	Live     *Live         // optional live counters
}

// StallGap is the inter-packet gap that counts as a stall.
const StallGap = time.Second

// PlayResult is the outcome of one RTSP playback session.
type PlayResult struct {
	OK     bool   `json:"ok"`
	Phase  string `json:"phase,omitempty"` // connect | describe | setup | play | stream
	Status int    `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	Ended  string `json:"ended,omitempty"` // completed | cancelled | dropped
	Server string `json:"server,omitempty"`

	ConnectMS    int64 `json:"connectMs"`
	DescribeMS   int64 `json:"describeMs"`
	SetupMS      int64 `json:"setupMs"`
	PlayMS       int64 `json:"playMs"`      // PLAY request -> PLAY response
	HandshakeMS  int64 `json:"handshakeMs"` // dial -> PLAY response
	TTFBMS       int64 `json:"ttfbMs"`      // PLAY request -> first RTP packet
	StartMS      int64 `json:"startMs"`     // dial -> first RTP packet (what a viewer waits for)
	FirstFrameMS int64 `json:"firstFrameMs"`

	Tracks  int    `json:"tracks"`
	Codec   string `json:"codec,omitempty"`
	Bytes   uint64 `json:"bytes"`
	Packets uint64 `json:"packets"`
	Frames  uint64 `json:"frames"`
	Lost    uint64 `json:"lost"`

	DurationMS int64   `json:"durationMs"` // first packet -> last packet
	AvgKbps    float64 `json:"avgKbps"`
	FPS        float64 `json:"fps"`
	LossPct    float64 `json:"lossPct"`
	JitterMS   float64 `json:"jitterMs"`
	MaxGapMS   int64   `json:"maxGapMs"`
	Stalls     int     `json:"stalls"`

	// Relay delay: arrival time minus the wall-clock time the server stamped on the
	// packet through RTCP sender reports. Only filled when the server sends SRs.
	DelayAvgMS   float64 `json:"delayAvgMs"`
	DelayMaxMS   float64 `json:"delayMaxMs"`
	DelaySamples int     `json:"delaySamples"`
}

type rtspResp struct {
	status     int
	headers    map[string]string
	challenges []string
	body       []byte
}

type rtspClient struct {
	conn       net.Conn
	br         *bufio.Reader
	cseq       int
	session    string
	user, pass string
	challenges []string
	timeout    time.Duration
}

func (c *rtspClient) send(method, uri string, hdr map[string]string) error {
	c.cseq++
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: RestreamCamera/1.0\r\n", method, uri, c.cseq)
	if c.session != "" {
		sb.WriteString("Session: " + c.session + "\r\n")
	}
	for k, v := range hdr {
		sb.WriteString(k + ": " + v + "\r\n")
	}
	if len(c.challenges) > 0 && c.user != "" {
		if a := authorization(c.challenges, c.user, c.pass, uri, method); a != "" {
			sb.WriteString("Authorization: " + a + "\r\n")
		}
	}
	sb.WriteString("\r\n")
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	_, err := c.conn.Write([]byte(sb.String()))
	return err
}

// readResponse reads one text response (status line, headers and body).
func (c *rtspClient) readResponse() (rtspResp, error) {
	rep := rtspResp{headers: map[string]string{}}
	line, err := c.br.ReadString('\n')
	if err != nil {
		return rep, err
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "RTSP/") {
		return rep, errors.New("malformed status line")
	}
	rep.status, _ = strconv.Atoi(parts[1])
	for {
		l, err := c.br.ReadString('\n')
		if err != nil {
			return rep, err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if k == "www-authenticate" {
			rep.challenges = append(rep.challenges, v)
		}
		if _, dup := rep.headers[k]; !dup {
			rep.headers[k] = v
		}
	}
	if n, _ := strconv.Atoi(rep.headers["content-length"]); n > 0 {
		if n > 1<<20 {
			return rep, errors.New("response body too large")
		}
		rep.body = make([]byte, n)
		if _, err := io.ReadFull(c.br, rep.body); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// do sends a request and reads its response, answering a 401 challenge once.
func (c *rtspClient) do(method, uri string, hdr map[string]string) (rtspResp, error) {
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if err := c.send(method, uri, hdr); err != nil {
		return rtspResp{}, err
	}
	rep, err := c.readResponse()
	if err != nil {
		return rep, err
	}
	if rep.status == 401 && c.user != "" && len(rep.challenges) > 0 && len(c.challenges) == 0 {
		c.challenges = rep.challenges
		if err := c.send(method, uri, hdr); err != nil {
			return rep, err
		}
		return c.readResponse()
	}
	return rep, nil
}

type sdpMedia struct {
	kind    string // video | audio | ...
	control string
	codec   string
	clock   int
}

func parseSDP(sdp string) []sdpMedia {
	var out []sdpMedia
	var cur *sdpMedia
	firstPT := 0
	for _, raw := range strings.Split(sdp, "\n") {
		l := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(l, "m="):
			f := strings.Fields(l[2:])
			if len(f) < 4 {
				continue
			}
			out = append(out, sdpMedia{kind: f[0]})
			cur = &out[len(out)-1]
			firstPT, _ = strconv.Atoi(f[3])
			if firstPT == 0 || firstPT == 8 {
				cur.clock = 8000
			}
			if firstPT == 26 {
				cur.codec, cur.clock = "JPEG", 90000
			}
		case cur != nil && strings.HasPrefix(l, "a=control:"):
			cur.control = strings.TrimSpace(l[len("a=control:"):])
		case cur != nil && strings.HasPrefix(l, "a=rtpmap:"):
			f := strings.Fields(l[len("a=rtpmap:"):])
			if len(f) == 2 {
				if pt, _ := strconv.Atoi(f[0]); pt == firstPT {
					enc := strings.Split(f[1], "/")
					cur.codec = enc[0]
					if len(enc) > 1 {
						cur.clock, _ = strconv.Atoi(enc[1])
					}
				}
			}
		}
	}
	return out
}

func trackURL(base, control string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(control), "rtsp://") || strings.HasPrefix(strings.ToLower(control), "rtsps://"):
		return control
	case control == "" || control == "*":
		return base
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + control
}

type trackState struct {
	media    sdpMedia
	video    bool
	started  bool
	lastSeq  uint16
	lost     uint64
	received uint64

	// RFC 3550 interarrival jitter, in RTP clock units
	jitter      float64
	prevArrival float64
	prevTS      uint32
	haveTransit bool
	srValid     bool
	srWall      time.Time
	srRTP       uint32
}

// Play connects, plays the stream over interleaved TCP and measures what a viewer would see.
func Play(ctx context.Context, opt PlayOptions) PlayResult {
	res := PlayResult{}
	fail := func(phase, msg string) PlayResult {
		res.Phase, res.Error = phase, msg
		return res
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	live := opt.Live
	if live == nil {
		live = &Live{}
	}
	u, err := url.Parse(opt.URL)
	if err != nil {
		return fail("connect", "invalid url: "+err.Error())
	}
	if u.Scheme != "rtsp" {
		return fail("connect", "unsupported scheme "+u.Scheme+" (stream mode needs rtsp://)")
	}
	user, pass := opt.User, opt.Pass
	if user == "" && u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "554")
	}
	clean := *u
	clean.User, clean.Fragment = nil, ""
	uri := clean.String()

	t0 := time.Now()
	dctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", host)
	cancel()
	if err != nil {
		return fail("connect", "dial: "+err.Error())
	}
	defer conn.Close()
	stopAfter := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopAfter()
	res.ConnectMS = time.Since(t0).Milliseconds()

	c := &rtspClient{conn: conn, br: bufio.NewReaderSize(conn, 64<<10), user: user, pass: pass, timeout: opt.Timeout}
	ctxErr := func(err error) string {
		if ctx.Err() != nil {
			return "cancelled"
		}
		return err.Error()
	}

	// DESCRIBE
	t := time.Now()
	rep, err := c.do("DESCRIBE", uri, map[string]string{"Accept": "application/sdp"})
	res.DescribeMS = time.Since(t).Milliseconds()
	if err != nil {
		return fail("describe", ctxErr(err))
	}
	res.Status = rep.status
	res.Server = rep.headers["server"]
	if rep.status != 200 {
		switch rep.status {
		case 401:
			return fail("describe", "unauthorized (401)")
		case 404:
			return fail("describe", "path not found (404)")
		}
		return fail("describe", fmt.Sprintf("rtsp status %d", rep.status))
	}
	medias := parseSDP(string(rep.body))
	if len(medias) == 0 {
		return fail("describe", "SDP has no media")
	}
	base := rep.headers["content-base"]
	if base == "" {
		base = rep.headers["content-location"]
	}
	if base == "" {
		base = uri
	}

	// SETUP every track over interleaved TCP
	tracks := make([]*trackState, 0, len(medias))
	t = time.Now()
	for i, m := range medias {
		hdr := map[string]string{"Transport": fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", i*2, i*2+1)}
		rep, err := c.do("SETUP", trackURL(base, m.control), hdr)
		if err != nil {
			return fail("setup", ctxErr(err))
		}
		if rep.status != 200 {
			return fail("setup", fmt.Sprintf("SETUP status %d", rep.status))
		}
		if s := rep.headers["session"]; s != "" && c.session == "" {
			c.session = strings.TrimSpace(strings.Split(s, ";")[0])
		}
		clock := m.clock
		if clock <= 0 {
			clock = 90000
		}
		m.clock = clock
		tracks = append(tracks, &trackState{media: m, video: m.kind == "video"})
		if m.kind == "video" && res.Codec == "" {
			res.Codec = m.codec
		}
	}
	res.SetupMS = time.Since(t).Milliseconds()
	res.Tracks = len(tracks)

	// PLAY
	tPlay := time.Now()
	rep, err = c.do("PLAY", base, map[string]string{"Range": "npt=0.000-"})
	res.PlayMS = time.Since(tPlay).Milliseconds()
	if err != nil {
		return fail("play", ctxErr(err))
	}
	if rep.status != 200 {
		return fail("play", fmt.Sprintf("PLAY status %d", rep.status))
	}
	res.HandshakeMS = time.Since(t0).Milliseconds()
	live.Handshake.Store(res.HandshakeMS + 1)
	live.Playing.Store(true)
	defer live.Playing.Store(false)

	// Media loop
	var (
		endAt         time.Time
		first, last   time.Time
		firstFrameSet bool
		bytes         uint64
		maxGap        time.Duration
		stalls        int
		lastKeepalive = time.Now()
		buf           = make([]byte, 65535)
		delaySum      float64
		delayMax      float64
		delayN        int
		streamErr     string
	)
	if opt.Duration > 0 {
		endAt = tPlay.Add(opt.Duration)
	}
	ended := "completed"
	var vtrack *trackState
	for _, tr := range tracks {
		if tr.video {
			vtrack = tr
			break
		}
	}

loop:
	for {
		now := time.Now()
		if ctx.Err() != nil {
			ended = "cancelled"
			break
		}
		if !endAt.IsZero() && !now.Before(endAt) {
			break
		}
		dl := now.Add(opt.Timeout)
		if !endAt.IsZero() && endAt.Before(dl) {
			dl = endAt
		}
		_ = conn.SetReadDeadline(dl)
		if now.Sub(lastKeepalive) > 15*time.Second {
			lastKeepalive = now
			_ = c.send("GET_PARAMETER", base, nil)
		}

		b, err := c.br.ReadByte()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				ended = "cancelled"
			case isTimeout(err) && !endAt.IsZero() && !time.Now().Before(endAt.Add(-50*time.Millisecond)):
				// normal end of the test window
			case isTimeout(err):
				ended, streamErr = "dropped", fmt.Sprintf("stalled: no data for %s", opt.Timeout)
			default:
				ended, streamErr = "dropped", "connection lost: "+shortErr(err)
			}
			break loop
		}
		if b != '$' {
			// A text response (keep-alive answer): consume it and carry on.
			if b != 'R' {
				ended, streamErr = "dropped", "protocol desync"
				break loop
			}
			_ = c.br.UnreadByte()
			if _, err := c.readResponse(); err != nil {
				ended, streamErr = "dropped", "connection lost: "+shortErr(err)
				break loop
			}
			continue
		}
		var hdr [3]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			ended, streamErr = "dropped", "connection lost: "+shortErr(err)
			break loop
		}
		ch, n := int(hdr[0]), int(binary.BigEndian.Uint16(hdr[1:]))
		if _, err := io.ReadFull(c.br, buf[:n]); err != nil {
			ended, streamErr = "dropped", "connection lost: "+shortErr(err)
			break loop
		}
		at := time.Now()
		bytes += uint64(n + 4)
		live.Bytes.Add(uint64(n + 4))
		idx := ch / 2
		if idx >= len(tracks) {
			continue
		}
		tr := tracks[idx]
		p := buf[:n]

		if ch%2 == 1 { // RTCP
			if tr.video {
				parseSenderReport(p, tr)
			}
			continue
		}
		if n < 12 {
			continue
		}
		res.Packets++
		live.Packets.Add(1)
		if first.IsZero() {
			first = at
			res.TTFBMS = at.Sub(tPlay).Milliseconds()
			res.StartMS = at.Sub(t0).Milliseconds()
			live.TTFB.Store(res.TTFBMS + 1)
			live.Start.Store(res.StartMS + 1)
		} else if gap := at.Sub(last); gap > 0 {
			if gap > maxGap {
				maxGap = gap
			}
			if gap > StallGap {
				stalls++
				live.Stalls.Add(1)
			}
		}
		last = at

		seq := binary.BigEndian.Uint16(p[2:])
		ts := binary.BigEndian.Uint32(p[4:])
		marker := p[1]&0x80 != 0
		tr.received++
		if !tr.started {
			tr.started, tr.lastSeq = true, seq
		} else {
			diff := int(int16(seq - tr.lastSeq))
			switch {
			case diff > 1 && diff < 1000:
				tr.lost += uint64(diff - 1)
				live.Lost.Add(uint64(diff - 1))
				tr.lastSeq = seq
			case diff >= 1000:
				tr.lastSeq = seq // sequence jump: resync, not loss
			case diff == 1:
				tr.lastSeq = seq
			case diff < 0 && tr.lost > 0:
				tr.lost-- // late packet filled a gap
			}
		}
		if tr.video {
			if marker {
				res.Frames++
				live.Frames.Add(1)
				if !firstFrameSet {
					firstFrameSet = true
					res.FirstFrameMS = at.Sub(tPlay).Milliseconds()
				}
				if tr.srValid {
					d := float64(int32(ts-tr.srRTP)) / float64(tr.media.clock)
					wall := tr.srWall.Add(time.Duration(d * float64(time.Second)))
					ms := float64(at.Sub(wall)) / float64(time.Millisecond)
					if ms > -60000 && ms < 60000 {
						delaySum += ms
						delayN++
						if ms > delayMax {
							delayMax = ms
						}
					}
				}
			}
			arrival := at.Sub(first).Seconds() * float64(tr.media.clock)
			if tr.haveTransit {
				d := (arrival - tr.prevArrival) - float64(int32(ts-tr.prevTS))
				tr.jitter += (math.Abs(d) - tr.jitter) / 16
			}
			tr.prevArrival, tr.prevTS, tr.haveTransit = arrival, ts, true
		}
	}

	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = c.send("TEARDOWN", base, nil)

	res.Bytes = bytes
	var lost, recv uint64
	for _, tr := range tracks {
		lost += tr.lost
		recv += tr.received
	}
	res.Lost = lost
	if recv+lost > 0 {
		res.LossPct = float64(lost) / float64(recv+lost) * 100
	}
	if !first.IsZero() {
		res.DurationMS = last.Sub(first).Milliseconds()
		if secs := last.Sub(first).Seconds(); secs > 0.5 {
			res.AvgKbps = float64(bytes) * 8 / 1000 / secs
			res.FPS = float64(res.Frames) / secs
		}
	}
	if vtrack != nil {
		res.JitterMS = vtrack.jitter / float64(vtrack.media.clock) * 1000
	}
	res.MaxGapMS = maxGap.Milliseconds()
	res.Stalls = stalls
	if delayN > 0 {
		res.DelayAvgMS, res.DelayMaxMS, res.DelaySamples = delaySum/float64(delayN), delayMax, delayN
	}
	res.Ended = ended
	switch {
	case first.IsZero():
		res.Phase = "stream"
		res.Error = "no media received"
		if streamErr != "" {
			res.Error = streamErr
		}
	case streamErr != "":
		res.Phase, res.Error = "stream", streamErr
	default:
		res.OK = true
	}
	return res
}

// parseSenderReport reads the NTP/RTP timestamp pair out of an RTCP compound packet.
func parseSenderReport(p []byte, tr *trackState) {
	for len(p) >= 4 {
		size := (int(binary.BigEndian.Uint16(p[2:])) + 1) * 4
		if size > len(p) {
			return
		}
		if p[1] == 200 && size >= 20 { // SR
			sec := binary.BigEndian.Uint32(p[8:])
			frac := binary.BigEndian.Uint32(p[12:])
			if sec > 2208988800 { // sane NTP time (after 1970)
				nanos := int64(float64(frac) / (1 << 32) * 1e9)
				tr.srWall = time.Unix(int64(sec)-2208988800, nanos)
				tr.srRTP = binary.BigEndian.Uint32(p[16:])
				tr.srValid = true
			}
			return
		}
		p = p[size:]
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func shortErr(err error) string {
	if errors.Is(err, io.EOF) {
		return "closed by server (EOF)"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}
