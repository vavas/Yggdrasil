package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMulawKnownVectors(t *testing.T) {
	cases := []struct {
		in  int16
		out byte
	}{
		{0, 0xff},
		{1000, 0xce},
		{-1000, 0x4e},
		{8000, 0xa0},
		{-8000, 0x20},
		{16000, 0x90},
	}
	for _, tc := range cases {
		if got := mulawEncode(tc.in); got != tc.out {
			t.Errorf("mulawEncode(%d) = %#x, want %#x", tc.in, got, tc.out)
		}
	}
	if snr := roundtripSNR(false); snr < 30 {
		t.Errorf("μ-law round trip SNR %.1f dB, want >= 30", snr)
	}
	if snr := roundtripSNR(true); snr < 30 {
		t.Errorf("A-law round trip SNR %.1f dB, want >= 30", snr)
	}
}

func roundtripSNR(alaw bool) float64 {
	var sig, err float64
	for i := 0; i < 160; i++ {
		s := int16(8000 * math.Sin(2*math.Pi*float64(i)*1000/8000))
		var d int16
		if alaw {
			d = alawDecode(alawEncode(s))
		} else {
			d = mulawDecode(mulawEncode(s))
		}
		sig += float64(s) * float64(s)
		diff := float64(s) - float64(d)
		err += diff * diff
	}
	if err == 0 {
		return 99
	}
	return 10 * math.Log10(sig/err)
}

func TestPromptRoundTripCorrelation(t *testing.T) {
	g, e, u, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	for _, clip := range [][]int16{g, e, u} {
		rt := make([]int16, len(clip))
		for i, s := range clip {
			rt[i] = mulawDecode(mulawEncode(s))
		}
		if score := bestCorr(rt, clip); score < 0.95 {
			t.Errorf("μ-law round trip correlation %.3f, want >= 0.95", score)
		}
	}
	if score := bestCorr(e, g); score > 0.8 {
		t.Errorf("english clip matched the greeting too well (%.3f)", score)
	}
	if score := bestCorr(u, g); score > 0.8 {
		t.Errorf("ukrainian clip matched the greeting too well (%.3f)", score)
	}
}

func TestDTMFDetector(t *testing.T) {
	if got := decodeDTMF(synthDTMF(1, 205)); got != 1 {
		t.Fatalf("block digit 1 decoded as %d", got)
	}
	if got := decodeDTMF(synthDTMF(2, 205)); got != 2 {
		t.Fatalf("block digit 2 decoded as %d", got)
	}
	if got := decodeDTMF(make([]int16, 205)); got != -1 {
		t.Fatalf("silence decoded as %d", got)
	}
	tone := make([]int16, 205)
	for i := range tone {
		tone[i] = int16(12000 * math.Sin(2*math.Pi*697*float64(i)/8000))
	}
	if got := decodeDTMF(tone); got != -1 {
		t.Fatalf("single tone decoded as %d", got)
	}

	s := &server{}
	c := &call{state: stUp, phase: phWait}
	c.dtmf.digit = -1
	c.dtmf.armed = true
	s.feedDTMF(c, synthDTMF(1, 1200))
	if !c.digitTaken {
		t.Fatal("in-band digit 1 was not accepted")
	}

	g, _, _, err := loadPrompts()
	if err != nil {
		t.Fatal(err)
	}
	c = &call{state: stUp, phase: phWait}
	c.dtmf.digit = -1
	c.dtmf.armed = true
	s.greeting = g
	s.feedDTMF(c, g)
	if c.digitTaken || c.replayed {
		t.Fatalf("greeting audio looked like a digit (taken=%v replayed=%v digit=%d)", c.digitTaken, c.replayed, c.dtmf.digit)
	}
}

func TestParseSIPAndSDP(t *testing.T) {
	raw := "INVITE sip:ivr@127.0.0.1 SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:9;branch=z9hG4bK1;rport\r\n" +
		"From: \"A\" <sip:a@127.0.0.1>;tag=t\r\n" +
		"To: <sip:ivr@127.0.0.1>\r\n" +
		"Call-ID: abc\r\n" +
		"CSeq: 7 INVITE\r\n" +
		"Contact: <sip:a@127.0.0.1:9>\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Content-Length: 0\r\n\r\n"
	// Content-Length 0 with a real body below would trim it; check headers first.
	m, err := parseSIP([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.method != "INVITE" || m.get("call-id") != "abc" || m.get("cseq") != "7 INVITE" {
		t.Fatalf("parsed %+v", m)
	}
	if got := sipURI(m.get("contact")); got != "sip:a@127.0.0.1:9" {
		t.Fatalf("contact uri %q", got)
	}
	body := "v=0\r\no=- 1 1 IN IP4 10.0.0.2\r\ns=-\r\nc=IN IP4 10.0.0.2\r\nt=0 0\r\n" +
		"m=audio 4002 RTP/AVP 8 101\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\n"
	raw = strings.Replace(raw, "Content-Length: 0", fmt.Sprintf("Content-Length: %d", len(body)), 1) + body
	m, err = parseSIP([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	offer, err := parseSDP(m.body)
	if err != nil {
		t.Fatal(err)
	}
	pt, alaw, ok := pickCodec(offer)
	if !ok || !alaw || pt != 8 || offer.dtmfPT != 101 || offer.port != 4002 || offer.ip != "10.0.0.2" {
		t.Fatalf("offer %+v pt %d alaw %v", offer, pt, alaw)
	}
	resp := formatResponse(m, 200, "OK", "xyz", "Contact: <sip:ivr@127.0.0.1:5060>\r\n", "v=0\r\n")
	if !bytes.Contains(resp, []byte("To: <sip:ivr@127.0.0.1>;tag=xyz\r\n")) {
		t.Fatalf("response missing to-tag:\n%s", resp)
	}
	if !bytes.Contains(resp, []byte("Via: SIP/2.0/UDP 127.0.0.1:9;branch=z9hG4bK1;rport\r\n")) {
		t.Fatalf("response missing via:\n%s", resp)
	}
}

func TestIVRCalls(t *testing.T) {
	srv, err := newServer(0, 0, "127.0.0.1", net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = srv.serve()
		close(done)
	}()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not stop")
		}
	})

	sipAddr := srv.sipConn.LocalAddr().(*net.UDPAddr)
	t.Run("options", func(t *testing.T) {
		conn := udpBind(t)
		defer conn.Close()
		local := conn.LocalAddr().(*net.UDPAddr)
		msg := fmt.Sprintf(
			"OPTIONS sip:ivr@127.0.0.1:%d SIP/2.0\r\n"+
				"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bKopt\r\n"+
				"From: <sip:test@127.0.0.1>;tag=opt\r\n"+
				"To: <sip:ivr@127.0.0.1>\r\n"+
				"Call-ID: options-1\r\n"+
				"CSeq: 1 OPTIONS\r\n"+
				"Content-Length: 0\r\n\r\n",
			sipAddr.Port, local.Port)
		if _, err := conn.WriteToUDP([]byte(msg), sipAddr); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(buf[:n], []byte("SIP/2.0 200")) {
			t.Fatalf("OPTIONS response:\n%s", buf[:n])
		}
	})

	t.Run("english-pcmu-rfc2833", func(t *testing.T) {
		samples := placeCall(t, srv, callSpec{digit: 1, alaw: false, inband: false})
		assertHeard(t, samples, srv.greeting, "greeting")
		assertHeard(t, samples, srv.english, "english confirmation")
	})
	t.Run("ukrainian-pcma-rfc2833", func(t *testing.T) {
		samples := placeCall(t, srv, callSpec{digit: 2, alaw: true, inband: false})
		assertHeard(t, samples, srv.greeting, "greeting")
		assertHeard(t, samples, srv.ukrainian, "ukrainian confirmation")
	})
	t.Run("english-inband", func(t *testing.T) {
		samples := placeCall(t, srv, callSpec{digit: 1, alaw: false, inband: true})
		assertHeard(t, samples, srv.english, "english confirmation")
	})
	t.Run("busy", func(t *testing.T) {
		a := newCaller(t, srv)
		defer a.close()
		if code := a.invite("busy-a", false); code != 200 {
			t.Fatalf("first invite status %d", code)
		}
		b := newCaller(t, srv)
		defer b.close()
		if code := b.invite("busy-b", false); code != 486 {
			t.Fatalf("second invite status %d, want 486", code)
		}
		a.sendBye()
		// The server must be idle for a later call.
		c := newCaller(t, srv)
		defer c.close()
		if code := c.invite("busy-c", false); code != 200 {
			t.Fatalf("invite after hangup status %d", code)
		}
		c.sendBye()
	})
}

type callSpec struct {
	digit  int
	alaw   bool
	inband bool
}

func placeCall(t *testing.T, srv *server, spec callSpec) []int16 {
	t.Helper()
	c := newCaller(t, srv)
	defer c.close()
	if code := c.invite(fmt.Sprintf("call-%d-%v", spec.digit, spec.inband), spec.alaw); code != 200 {
		t.Fatalf("invite status %d\n%s", code, c.last)
	}
	// A real phone sends RTP immediately. That lets the server lock onto the
	// caller's address before it starts the greeting.
	c.primeRTP()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && c.sampleCount() < 8000 {
		time.Sleep(20 * time.Millisecond)
	}
	if c.sampleCount() < 8000 {
		t.Fatalf("only %d samples of greeting audio", c.sampleCount())
	}
	if spec.inband {
		c.sendInband(spec.digit, spec.alaw)
	} else {
		c.sendRFC2833(spec.digit)
	}
	if !c.waitBye(8 * time.Second) {
		t.Fatal("server did not send BYE")
	}
	time.Sleep(200 * time.Millisecond)
	return c.samples()
}

func assertHeard(t *testing.T, got, want []int16, name string) {
	t.Helper()
	score := bestCorr(got, want)
	if score < 0.85 {
		t.Errorf("did not hear %s (correlation %.3f, got %d samples, want %d)", name, score, len(got), len(want))
	}
}

type caller struct {
	t       *testing.T
	srv     *server
	sip     *net.UDPConn
	rtp     *net.UDPConn
	sipAddr *net.UDPAddr
	tag     string
	callID  string
	alaw    bool
	dtmfPT  int

	toHeader string
	last     string

	mu      sync.Mutex
	pcm     []int16
	stopped bool
}

func newCaller(t *testing.T, srv *server) *caller {
	t.Helper()
	c := &caller{
		t:       t,
		srv:     srv,
		sip:     udpBind(t),
		rtp:     udpBind(t),
		sipAddr: srv.sipConn.LocalAddr().(*net.UDPAddr),
		tag:     "caller",
	}
	go c.readRTP()
	return c
}

func (c *caller) close() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.sip.Close()
	c.rtp.Close()
}

func (c *caller) invite(callID string, alaw bool) int {
	c.callID = callID
	c.mu.Lock()
	c.alaw = alaw
	c.mu.Unlock()
	c.dtmfPT = 101
	local := c.sip.LocalAddr().(*net.UDPAddr)
	rtp := c.rtp.LocalAddr().(*net.UDPAddr)
	pt, name := 0, "PCMU"
	if alaw {
		pt, name = 8, "PCMA"
	}
	body := fmt.Sprintf(
		"v=0\r\n"+
			"o=- 1 1 IN IP4 127.0.0.1\r\n"+
			"s=test\r\n"+
			"c=IN IP4 127.0.0.1\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP %d 101\r\n"+
			"a=rtpmap:%d %s/8000\r\n"+
			"a=rtpmap:101 telephone-event/8000\r\n"+
			"a=fmtp:101 0-15\r\n"+
			"a=sendrecv\r\n"+
			"a=ptime:20\r\n",
		rtp.Port, pt, pt, name)
	msg := fmt.Sprintf(
		"INVITE sip:ivr@127.0.0.1:%d SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK%s;rport\r\n"+
			"Max-Forwards: 70\r\n"+
			"From: \"Test\" <sip:test@127.0.0.1:%d>;tag=%s\r\n"+
			"To: <sip:ivr@127.0.0.1>\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 1 INVITE\r\n"+
			"Contact: <sip:test@127.0.0.1:%d>\r\n"+
			"Content-Type: application/sdp\r\n"+
			"Content-Length: %d\r\n\r\n%s",
		c.sipAddr.Port, local.Port, callID, local.Port, c.tag,
		callID, local.Port, len(body), body)
	if _, err := c.sip.WriteToUDP([]byte(msg), c.sipAddr); err != nil {
		c.t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pkt, err := c.readSIP(time.Until(deadline))
		if err != nil {
			c.t.Fatalf("waiting for invite response: %v", err)
		}
		if bytes.HasPrefix(pkt, []byte("SIP/2.0 100")) {
			continue
		}
		if bytes.HasPrefix(pkt, []byte("SIP/2.0 200")) {
			c.ack(pkt)
			return 200
		}
		if bytes.HasPrefix(pkt, []byte("SIP/2.0 ")) {
			code := 0
			fmt.Sscanf(string(pkt), "SIP/2.0 %d", &code)
			c.last = string(pkt)
			return code
		}
	}
	c.t.Fatal("no final response to INVITE")
	return 0
}

func (c *caller) ack(ok []byte) {
	to := headerValue(ok, "To")
	contact := headerValue(ok, "Contact")
	uri := sipURI(contact)
	if uri == "" {
		uri = fmt.Sprintf("sip:ivr@127.0.0.1:%d", c.sipAddr.Port)
	}
	local := c.sip.LocalAddr().(*net.UDPAddr)
	msg := fmt.Sprintf(
		"ACK %s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bKack%s;rport\r\n"+
			"Max-Forwards: 70\r\n"+
			"From: \"Test\" <sip:test@127.0.0.1:%d>;tag=%s\r\n"+
			"To: %s\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 1 ACK\r\n"+
			"Content-Length: 0\r\n\r\n",
		uri, local.Port, c.callID, local.Port, c.tag, to, c.callID)
	if _, err := c.sip.WriteToUDP([]byte(msg), c.sipAddr); err != nil {
		c.t.Fatal(err)
	}
	c.toHeader = to
}

func (c *caller) sendBye() {
	local := c.sip.LocalAddr().(*net.UDPAddr)
	uri := fmt.Sprintf("sip:ivr@127.0.0.1:%d", c.sipAddr.Port)
	msg := fmt.Sprintf(
		"BYE %s SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bKbye%s\r\n"+
			"Max-Forwards: 70\r\n"+
			"From: \"Test\" <sip:test@127.0.0.1:%d>;tag=%s\r\n"+
			"To: %s\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: 2 BYE\r\n"+
			"Content-Length: 0\r\n\r\n",
		uri, local.Port, c.callID, local.Port, c.tag, c.toHeader, c.callID)
	_, _ = c.sip.WriteToUDP([]byte(msg), c.sipAddr)
	_, _ = c.readSIP(time.Second) // 200 OK, best effort
}

func (c *caller) waitBye(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		pkt, err := c.readSIP(time.Until(deadline))
		if err != nil {
			return false
		}
		if bytes.HasPrefix(pkt, []byte("BYE ")) {
			c.okBye(pkt)
			return true
		}
		if bytes.HasPrefix(pkt, []byte("SIP/2.0 200")) && bytes.Contains(pkt, []byte("INVITE")) {
			c.ack(pkt)
		}
	}
	return false
}

func (c *caller) okBye(bye []byte) {
	resp := formatResponse(mustParse(c.t, bye), 200, "OK", "", "", "")
	_, _ = c.sip.WriteToUDP(resp, c.sipAddr)
}

func (c *caller) primeRTP() {
	dest := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: c.srv.rtpPort}
	pkt := make([]byte, 12+160)
	pkt[0] = 0x80
	c.mu.Lock()
	alaw := c.alaw
	c.mu.Unlock()
	if alaw {
		pkt[1] = 8
		for i := 12; i < len(pkt); i++ {
			pkt[i] = alawEncode(0)
		}
	} else {
		for i := 12; i < len(pkt); i++ {
			pkt[i] = 0xFF
		}
	}
	binary.BigEndian.PutUint32(pkt[8:], 0x33333333)
	_, _ = c.rtp.WriteToUDP(pkt, dest)
}

func (c *caller) sendRFC2833(digit int) {
	dest := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: c.srv.rtpPort}
	ssrc := uint32(0x11111111)
	seq := uint16(1)
	ts := uint32(0)
	dur := 0
	for i := 0; i < 10; i++ {
		dur += 160
		end := byte(0)
		if i == 9 {
			end = 0x80
		}
		pkt := make([]byte, 16)
		pkt[0] = 0x80
		pkt[1] = byte(c.dtmfPT)
		if i == 0 {
			pkt[1] |= 0x80
		}
		binary.BigEndian.PutUint16(pkt[2:], seq)
		binary.BigEndian.PutUint32(pkt[4:], ts)
		binary.BigEndian.PutUint32(pkt[8:], ssrc)
		pkt[12] = byte(digit)
		pkt[13] = end | 10
		binary.BigEndian.PutUint16(pkt[14:], uint16(dur))
		_, _ = c.rtp.WriteToUDP(pkt, dest)
		seq++
		ts += 160
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *caller) sendInband(digit int, alaw bool) {
	dest := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: c.srv.rtpPort}
	pcm := synthDTMF(digit, 160*20) // 400 ms
	pt := byte(0)
	if alaw {
		pt = 8
	}
	ssrc := uint32(0x22222222)
	seq := uint16(10)
	ts := uint32(0)
	for off := 0; off+160 <= len(pcm); off += 160 {
		pkt := make([]byte, 12+160)
		pkt[0] = 0x80
		pkt[1] = pt
		binary.BigEndian.PutUint16(pkt[2:], seq)
		binary.BigEndian.PutUint32(pkt[4:], ts)
		binary.BigEndian.PutUint32(pkt[8:], ssrc)
		for i := 0; i < 160; i++ {
			if alaw {
				pkt[12+i] = alawEncode(pcm[off+i])
			} else {
				pkt[12+i] = mulawEncode(pcm[off+i])
			}
		}
		_, _ = c.rtp.WriteToUDP(pkt, dest)
		seq++
		ts += 160
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *caller) readRTP() {
	buf := make([]byte, 2048)
	for {
		_ = c.rtp.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, _, err := c.rtp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 12 || buf[0]>>6 != 2 {
			continue
		}
		pt := buf[1] & 0x7f
		if pt >= 72 && pt <= 76 {
			continue
		}
		payload := append([]byte(nil), buf[12:n]...)
		c.mu.Lock()
		alaw := c.alaw
		stopped := c.stopped
		c.mu.Unlock()
		if stopped {
			return
		}
		pcm := make([]int16, len(payload))
		for i, b := range payload {
			if alaw {
				pcm[i] = alawDecode(b)
			} else {
				pcm[i] = mulawDecode(b)
			}
		}
		c.mu.Lock()
		if !c.stopped {
			c.pcm = append(c.pcm, pcm...)
		}
		c.mu.Unlock()
	}
}

func (c *caller) sampleCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pcm)
}

func (c *caller) samples() []int16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int16, len(c.pcm))
	copy(out, c.pcm)
	return out
}

func (c *caller) readSIP(d time.Duration) ([]byte, error) {
	if d <= 0 {
		d = time.Millisecond
	}
	buf := make([]byte, 65535)
	_ = c.sip.SetReadDeadline(time.Now().Add(d))
	n, _, err := c.sip.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func udpBind(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func headerValue(msg []byte, name string) string {
	m, err := parseSIP(msg)
	if err != nil {
		return ""
	}
	return m.get(strings.ToLower(name))
}

func mustParse(t *testing.T, msg []byte) *sipMsg {
	t.Helper()
	m, err := parseSIP(msg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func synthDTMF(digit, n int) []int16 {
	low := map[int]float64{1: 697, 2: 697, 3: 697, 4: 770, 5: 770, 6: 770, 7: 852, 8: 852, 9: 852, 0: 941}
	high := map[int]float64{1: 1209, 2: 1336, 3: 1477, 4: 1209, 5: 1336, 6: 1477, 7: 1209, 8: 1336, 9: 1477, 0: 1336}
	f1, f2 := low[digit], high[digit]
	out := make([]int16, n)
	for i := range out {
		s := 0.30*math.Sin(2*math.Pi*f1*float64(i)/8000) + 0.30*math.Sin(2*math.Pi*f2*float64(i)/8000)
		out[i] = int16(s * 32767)
	}
	return out
}

func bestCorr(hay, needle []int16) float64 {
	start := 0
	for start < len(needle) && needle[start] > -200 && needle[start] < 200 {
		start++
	}
	if start >= len(needle) {
		start = 0
	}
	// A few hundred milliseconds is enough to identify the phrase, and it
	// still fits in the audio captured before a digit interrupts the greeting.
	window := 4000
	if start+window > len(needle) {
		window = len(needle) - start
	}
	if window < 400 || len(hay) < window {
		return 0
	}
	nd := needle[start : start+window]
	bestLag := 0
	best := -1.0
	for lag := 0; lag+window <= len(hay); lag += 40 {
		if c := normCorr(hay[lag:lag+window], nd); c > best {
			best = c
			bestLag = lag
		}
	}
	from := bestLag - 40
	if from < 0 {
		from = 0
	}
	to := bestLag + 40
	if to+window > len(hay) {
		to = len(hay) - window
	}
	for lag := from; lag <= to; lag++ {
		if c := normCorr(hay[lag:lag+window], nd); c > best {
			best = c
		}
	}
	return best
}

func normCorr(a, b []int16) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var ma, mb float64
	for i := 0; i < n; i++ {
		ma += float64(a[i])
		mb += float64(b[i])
	}
	ma /= float64(n)
	mb /= float64(n)
	var num, da, db float64
	for i := 0; i < n; i++ {
		x := float64(a[i]) - ma
		y := float64(b[i]) - mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}
