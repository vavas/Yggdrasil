// Command yggdrasil is a one-call SIP IVR.
//
//	go run .
//
// It listens on UDP :5060, answers one call, and plays a woman's voice:
// "For English, press 1; for Ukrainian, press 2." Press 1 or 2 and it
// confirms that language, then hangs up.
//
// SIP over UDP is used instead of WebRTC: a correct-enough user agent is
// just the standard library, while WebRTC needs ICE, DTLS, and SRTP.
// Point a softphone at sip:ivr@<this-host>:5060 (UDP, PCMU or PCMA, RFC 2833
// or in-band DTMF). There is no registrar and no authentication; this is a
// direct answering endpoint.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	stWaitAck = iota + 1
	stUp
	stByeSent
)

const (
	phPlay = iota + 1
	phWait
	phDrain
)

const (
	afterWait = iota + 1
	afterBye
)

const frameSamples = 160 // 20 ms at 8 kHz

func main() {
	sipPort := flag.Int("sip", 5060, "SIP UDP port")
	rtpPort := flag.Int("rtp", 4000, "RTP UDP port")
	ip := flag.String("ip", "", "IPv4 address to put in Contact and SDP (default: auto)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: go run . [-sip port] [-rtp port] [-ip addr]\n\n")
		fmt.Fprintf(os.Stderr, "Answer a SIP call and play an English/Ukrainian IVR greeting.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if err := serve(*sipPort, *rtpPort, *ip, net.IPv4zero); err != nil {
		log.Fatal(err)
	}
}

// serve binds SIP and RTP and blocks until the sockets close.
func serve(sipPort, rtpPort int, advertise string, bindIP net.IP) error {
	if sipPort != 0 && sipPort == rtpPort {
		return fmt.Errorf("SIP and RTP ports must differ")
	}
	srv, err := newServer(sipPort, rtpPort, advertise, bindIP)
	if err != nil {
		return err
	}
	defer srv.Close()
	log.Printf("listening SIP udp %s  RTP udp %s", srv.sipConn.LocalAddr(), srv.rtpConn.LocalAddr())
	log.Printf("call sip:ivr@<host>:%d   (UDP, no registration)", srv.sipPort)
	log.Printf("greeting: for English press 1, for Ukrainian press 2")
	return srv.serve()
}

type server struct {
	sipPort   int
	rtpPort   int
	advertise string
	routeIP   string

	greeting  []int16
	english   []int16
	ukrainian []int16

	sipConn *net.UDPConn
	rtpConn *net.UDPConn

	mu   sync.Mutex
	call *call

	stop chan struct{}
	once sync.Once
}

type call struct {
	state       int
	phase       int
	peer        *net.UDPAddr
	callID      string
	from        string
	to          string
	toTag       string
	remoteURI   string
	inviteCSeq  int
	localIP     string
	audioPT     int
	dtmfPT      int
	alaw        bool
	rtpDest     *net.UDPAddr
	latched     bool
	seq         uint16
	ts          uint32
	ssrc        uint32
	marker      bool
	clip        []int16
	clipPos     int
	after       int
	drain       int
	hangup      bool
	replayed    bool
	digitTaken  bool
	waitUntil   time.Time
	greetBy     time.Time
	nextRTP     time.Time
	ok200       []byte
	nextRetx    time.Time
	retxWait    time.Duration
	ackDeadline time.Time
	byeMsg      []byte
	byeTries    int
	nextBye     time.Time
	dtmf        dtmfDet
}

type dtmfDet struct {
	buf    []int16
	digit  int
	run    int
	gap    int
	armed  bool
	evOn   bool
	evID   int
	evDur  int
	evTake bool
}

func newServer(sipPort, rtpPort int, advertise string, bindIP net.IP) (*server, error) {
	greeting, english, ukrainian, err := loadPrompts()
	if err != nil {
		return nil, err
	}
	if bindIP == nil {
		bindIP = net.IPv4zero
	}
	sipConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP, Port: sipPort})
	if err != nil {
		return nil, fmt.Errorf("sip listen: %w", err)
	}
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP, Port: rtpPort})
	if err != nil {
		sipConn.Close()
		return nil, fmt.Errorf("rtp listen: %w", err)
	}
	_ = sipConn.SetReadBuffer(1 << 20)
	_ = rtpConn.SetReadBuffer(1 << 20)
	s := &server{
		sipPort:   sipConn.LocalAddr().(*net.UDPAddr).Port,
		rtpPort:   rtpConn.LocalAddr().(*net.UDPAddr).Port,
		advertise: advertise,
		routeIP:   routeIPv4(),
		greeting:  greeting,
		english:   english,
		ukrainian: ukrainian,
		sipConn:   sipConn,
		rtpConn:   rtpConn,
		stop:      make(chan struct{}),
	}
	return s, nil
}

func (s *server) Close() {
	s.once.Do(func() {
		close(s.stop)
		s.sipConn.Close()
		s.rtpConn.Close()
	})
}

func (s *server) serve() error {
	errc := make(chan error, 3)
	go func() { errc <- s.sipLoop() }()
	go func() { errc <- s.rtpLoop() }()
	go func() { s.mediaLoop(); errc <- nil }()
	var err error
	for i := 0; i < 3; i++ {
		if e := <-errc; e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (s *server) sipLoop() error {
	buf := make([]byte, 65535)
	for {
		n, addr, err := s.sipConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stop:
				return nil
			default:
				return err
			}
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		s.handleSIP(pkt, cloneAddr(addr))
	}
}

func (s *server) rtpLoop() error {
	buf := make([]byte, 2048)
	for {
		n, addr, err := s.rtpConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stop:
				return nil
			default:
				return err
			}
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		s.handleRTP(pkt, cloneAddr(addr))
	}
}

func (s *server) mediaLoop() {
	for {
		s.pump()
		delay := 20 * time.Millisecond
		s.mu.Lock()
		if s.call != nil && s.call.state == stUp {
			if d := time.Until(s.call.nextRTP); d < delay {
				delay = d
			}
		}
		s.mu.Unlock()
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-s.stop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (s *server) pump() {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.call
	if c == nil {
		return
	}
	now := time.Now()
	switch c.state {
	case stWaitAck:
		if now.After(c.ackDeadline) {
			log.Printf("no ACK, dropping call %s", c.callID)
			s.call = nil
			return
		}
		if !now.Before(c.nextRetx) && len(c.ok200) > 0 {
			_, _ = s.sipConn.WriteToUDP(c.ok200, c.peer)
			if c.retxWait < 4*time.Second {
				c.retxWait *= 2
			}
			c.nextRetx = now.Add(c.retxWait)
		}
	case stUp:
		// Start the greeting once the caller sends RTP (symmetric RTP / NAT),
		// or after a short wait if they never do and the SDP address is enough.
		if c.phase == 0 && (c.latched || !now.Before(c.greetBy)) {
			log.Printf("playing greeting")
			s.startClip(c, s.greeting, afterWait)
		}
		for n := 0; n < 5 && !now.Before(c.nextRTP); n++ {
			if c.rtpDest == nil {
				c.nextRTP = c.nextRTP.Add(20 * time.Millisecond)
				continue
			}
			pkt := s.fillRTP(c)
			_, _ = s.rtpConn.WriteToUDP(pkt, c.rtpDest)
			c.nextRTP = c.nextRTP.Add(20 * time.Millisecond)
			if c.hangup {
				c.hangup = false
				s.sendBye(c)
				break
			}
		}
		if c.state == stUp && now.After(c.nextRTP.Add(200*time.Millisecond)) {
			c.nextRTP = now
		}
		if c.state == stUp && c.phase == phWait && !c.waitUntil.IsZero() && !now.Before(c.waitUntil) {
			s.onTimeout(c)
		}
	case stByeSent:
		if c.byeTries < 5 && !now.Before(c.nextBye) {
			_, _ = s.sipConn.WriteToUDP(c.byeMsg, c.peer)
			c.byeTries++
			c.nextBye = now.Add(500 * time.Millisecond)
		} else if c.byeTries >= 5 && !now.Before(c.nextBye) {
			log.Printf("BYE unanswered, clearing %s", c.callID)
			s.call = nil
		}
	}
}

func (s *server) fillRTP(c *call) []byte {
	frame := make([]int16, frameSamples)
	switch c.phase {
	case phPlay:
		if c.clipPos < len(c.clip) {
			n := len(c.clip) - c.clipPos
			if n > frameSamples {
				n = frameSamples
			}
			copy(frame, c.clip[c.clipPos:c.clipPos+n])
			c.clipPos += n
		}
		if c.clipPos >= len(c.clip) {
			s.finishClip(c)
		}
	case phDrain:
		c.drain--
		if c.drain <= 0 {
			c.hangup = true
		}
	}
	return s.encodeRTP(c, frame)
}

func (s *server) encodeRTP(c *call, frame []int16) []byte {
	pkt := make([]byte, 12+frameSamples)
	pkt[0] = 0x80
	pt := byte(c.audioPT & 0x7f)
	if c.marker {
		pkt[1] = 0x80 | pt
		c.marker = false
	} else {
		pkt[1] = pt
	}
	binary.BigEndian.PutUint16(pkt[2:], c.seq)
	binary.BigEndian.PutUint32(pkt[4:], c.ts)
	binary.BigEndian.PutUint32(pkt[8:], c.ssrc)
	for i, sample := range frame {
		if c.alaw {
			pkt[12+i] = alawEncode(sample)
		} else {
			pkt[12+i] = mulawEncode(sample)
		}
	}
	c.seq++
	c.ts += frameSamples
	return pkt
}

func (s *server) finishClip(c *call) {
	switch c.after {
	case afterBye:
		c.phase = phDrain
		c.drain = 15 // 300 ms of silence so the last word is heard
		log.Printf("confirmation finished, hanging up")
	default:
		c.phase = phWait
		c.waitUntil = time.Now().Add(12 * time.Second)
		log.Printf("greeting finished, waiting for a digit")
	}
	c.clip = nil
	c.clipPos = 0
}

func (s *server) startClip(c *call, pcm []int16, after int) {
	c.clip = pcm
	c.clipPos = 0
	c.after = after
	c.phase = phPlay
	c.marker = true
	c.hangup = false
	c.drain = 0
}

func (s *server) onTimeout(c *call) {
	if c.digitTaken {
		return
	}
	if !c.replayed {
		c.replayed = true
		log.Printf("no digit, replaying the greeting")
		s.startClip(c, s.greeting, afterWait)
		return
	}
	log.Printf("no digit, hanging up")
	s.sendBye(c)
}

func (s *server) onDigit(c *call, digit int, how string) {
	if c == nil || c.state != stUp || c.digitTaken {
		return
	}
	if c.phase == phDrain || c.hangup {
		return
	}
	if digit >= 0 {
		log.Printf("digit %d (%s)", digit, how)
	} else {
		log.Printf("non-digit tone (%s)", how)
	}
	switch digit {
	case 1:
		c.digitTaken = true
		log.Printf("confirming English")
		s.startClip(c, s.english, afterBye)
	case 2:
		c.digitTaken = true
		log.Printf("confirming Ukrainian")
		s.startClip(c, s.ukrainian, afterBye)
	default:
		if !c.replayed {
			c.replayed = true
			log.Printf("not 1 or 2, replaying the greeting")
			s.startClip(c, s.greeting, afterWait)
			return
		}
		log.Printf("not 1 or 2 again, hanging up")
		c.digitTaken = true
		c.phase = phDrain
		c.drain = 5
		c.clip = nil
		c.hangup = false
	}
}

func (s *server) sendBye(c *call) {
	if c.state != stUp {
		return
	}
	uri := c.remoteURI
	if uri == "" {
		uri = fmt.Sprintf("sip:%s", c.peer.IP.String())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "BYE %s SIP/2.0\r\n", uri)
	fmt.Fprintf(&b, "Via: SIP/2.0/UDP %s:%d;branch=z9hG4bK%s;rport\r\n", c.localIP, s.sipPort, newTag())
	b.WriteString("Max-Forwards: 70\r\n")
	fmt.Fprintf(&b, "From: %s\r\n", c.to)
	fmt.Fprintf(&b, "To: %s\r\n", c.from)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", c.callID)
	b.WriteString("CSeq: 1 BYE\r\n")
	b.WriteString("User-Agent: Yggdrasil-IVR/1.0\r\n")
	b.WriteString("Content-Length: 0\r\n\r\n")
	c.byeMsg = []byte(b.String())
	_, _ = s.sipConn.WriteToUDP(c.byeMsg, c.peer)
	c.state = stByeSent
	c.byeTries = 1
	c.nextBye = time.Now().Add(500 * time.Millisecond)
	log.Printf("BYE sent to %s", c.peer)
}

func (s *server) handleSIP(pkt []byte, addr *net.UDPAddr) {
	m, err := parseSIP(pkt)
	if err != nil {
		if len(pkt) > 0 {
			log.Printf("ignored malformed SIP from %s: %v", addr, err)
		}
		return
	}
	if m.isResp {
		s.onResponse(m)
		return
	}
	if missingDialogHeaders(m) {
		if m.get("via") != "" && m.get("from") != "" && m.get("call-id") != "" && m.get("cseq") != "" {
			s.reply(addr, m, 400, "Bad Request", newTag(), "", "")
		}
		return
	}
	switch strings.ToUpper(m.method) {
	case "INVITE":
		s.onInvite(m, addr)
	case "ACK":
		s.onAck(m)
	case "BYE":
		s.onBye(m, addr)
	case "CANCEL":
		s.onCancel(m, addr)
	case "INFO":
		s.onInfo(m, addr)
	case "OPTIONS":
		s.onOptions(m, addr)
	default:
		s.reply(addr, m, 405, "Method Not Allowed", newTag(),
			"Allow: INVITE, ACK, BYE, CANCEL, INFO, OPTIONS\r\n", "")
	}
}

func missingDialogHeaders(m *sipMsg) bool {
	return m.get("via") == "" || m.get("from") == "" || m.get("to") == "" || m.get("call-id") == "" || m.get("cseq") == ""
}

func (s *server) onInvite(m *sipMsg, addr *net.UDPAddr) {
	if req := strings.TrimSpace(m.get("require")); req != "" {
		s.reply(addr, m, 420, "Bad Extension", newTag(), "Unsupported: "+req+"\r\n", "")
		return
	}
	id := m.get("call-id")
	cseq, _ := parseCSeq(m.get("cseq"))

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.call != nil && s.call.callID == id {
		if s.call.state == stByeSent {
			s.reply(addr, m, 481, "Call/Transaction Does Not Exist", s.call.toTag, "", "")
			return
		}
		if s.call.inviteCSeq == cseq && len(s.call.ok200) > 0 {
			_, _ = s.sipConn.WriteToUDP(s.call.ok200, addr)
			return
		}
		s.updateRTP(s.call, m, addr)
		extra, body := s.answerExtras(s.call)
		s.call.ok200 = s.reply(addr, m, 200, "OK", s.call.toTag, extra, body)
		s.call.inviteCSeq = cseq
		log.Printf("re-INVITE %s", id)
		return
	}
	if s.call != nil {
		s.reply(addr, m, 486, "Busy Here", newTag(), "", "")
		log.Printf("busy, rejected %s", addr)
		return
	}

	offer, err := parseSDP(m.body)
	if err != nil || offer.port <= 0 {
		s.reply(addr, m, 488, "Not Acceptable Here", newTag(), "", "")
		log.Printf("invite from %s has no usable audio (%v)", addr, err)
		return
	}
	audioPT, alaw, ok := pickCodec(offer)
	if !ok {
		s.reply(addr, m, 488, "Not Acceptable Here", newTag(), "", "")
		log.Printf("invite from %s offered no PCMU or PCMA", addr)
		return
	}

	s.reply(addr, m, 100, "Trying", "", "", "")

	ip := s.advertiseFor(addr)
	tag := newTag()
	to := m.get("to")
	if !hasTag(to) {
		to = to + ";tag=" + tag
	}
	remote := sipURI(m.get("contact"))
	if remote == "" {
		remote = sipURI(m.get("from"))
	}
	rtpIP := offer.ip
	if rtpIP == "" || rtpIP == "0.0.0.0" || ip4(rtpIP) == nil {
		rtpIP = addr.IP.String()
	}
	c := &call{
		state:       stWaitAck,
		peer:        cloneAddr(addr),
		callID:      id,
		from:        m.get("from"),
		to:          to,
		toTag:       tag,
		remoteURI:   remote,
		inviteCSeq:  cseq,
		localIP:     ip,
		audioPT:     audioPT,
		dtmfPT:      offer.dtmfPT,
		alaw:        alaw,
		rtpDest:     &net.UDPAddr{IP: ip4(rtpIP), Port: offer.port},
		seq:         uint16(randU32()),
		ts:          randU32(),
		ssrc:        randU32(),
		ackDeadline: time.Now().Add(32 * time.Second),
		retxWait:    500 * time.Millisecond,
	}
	c.dtmf.digit = -1
	c.dtmf.armed = true
	extra, body := s.answerExtras(c)
	c.ok200 = s.reply(addr, m, 200, "OK", tag, extra, body)
	c.nextRetx = time.Now().Add(c.retxWait)
	s.call = c
	codec := "PCMU"
	if alaw {
		codec = "PCMA"
	}
	log.Printf("invite from %s call-id=%s codec=%s dtmf=%d rtp=%s:%d", addr, id, codec, offer.dtmfPT, rtpIP, offer.port)
}

func (s *server) answerExtras(c *call) (extra, body string) {
	body = s.sdpBody(c)
	extra = fmt.Sprintf(
		"Contact: <sip:ivr@%s:%d>\r\nAllow: INVITE, ACK, BYE, CANCEL, INFO, OPTIONS\r\nContent-Type: application/sdp\r\n",
		c.localIP, s.sipPort,
	)
	return extra, body
}

func (s *server) sdpBody(c *call) string {
	codec := "PCMU"
	if c.alaw {
		codec = "PCMA"
	}
	sess := time.Now().Unix()
	var b strings.Builder
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=- %d %d IN IP4 %s\r\n", sess, sess, c.localIP)
	b.WriteString("s=Yggdrasil\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", c.localIP)
	b.WriteString("t=0 0\r\n")
	if c.dtmfPT >= 0 {
		fmt.Fprintf(&b, "m=audio %d RTP/AVP %d %d\r\n", s.rtpPort, c.audioPT, c.dtmfPT)
	} else {
		fmt.Fprintf(&b, "m=audio %d RTP/AVP %d\r\n", s.rtpPort, c.audioPT)
	}
	fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\n", c.audioPT, codec)
	if c.dtmfPT >= 0 {
		fmt.Fprintf(&b, "a=rtpmap:%d telephone-event/8000\r\n", c.dtmfPT)
		fmt.Fprintf(&b, "a=fmtp:%d 0-15\r\n", c.dtmfPT)
	}
	b.WriteString("a=ptime:20\r\na=sendrecv\r\n")
	return b.String()
}

func (s *server) updateRTP(c *call, m *sipMsg, addr *net.UDPAddr) {
	offer, err := parseSDP(m.body)
	if err != nil || offer.port <= 0 {
		return
	}
	rtpIP := offer.ip
	if rtpIP == "" || rtpIP == "0.0.0.0" || ip4(rtpIP) == nil {
		rtpIP = addr.IP.String()
	}
	c.rtpDest = &net.UDPAddr{IP: ip4(rtpIP), Port: offer.port}
	c.latched = false
	if u := sipURI(m.get("contact")); u != "" {
		c.remoteURI = u
	}
	c.peer = cloneAddr(addr)
}

func (s *server) onAck(m *sipMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.call
	if c == nil || m.get("call-id") != c.callID || c.state != stWaitAck {
		return
	}
	c.state = stUp
	c.nextRTP = time.Now()
	c.greetBy = time.Now().Add(500 * time.Millisecond)
	log.Printf("ACK from %s", c.peer)
}

func (s *server) onBye(m *sipMsg, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.call == nil || m.get("call-id") != s.call.callID {
		s.reply(addr, m, 481, "Call/Transaction Does Not Exist", newTag(), "", "")
		return
	}
	s.reply(addr, m, 200, "OK", s.call.toTag, "", "")
	log.Printf("caller hung up")
	s.call = nil
}

func (s *server) onCancel(m *sipMsg, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.call == nil || m.get("call-id") != s.call.callID {
		s.reply(addr, m, 481, "Call/Transaction Does Not Exist", newTag(), "", "")
		return
	}
	// The call is answered immediately, so a late CANCEL does not end it.
	s.reply(addr, m, 200, "OK", "", "", "")
	log.Printf("CANCEL after answer ignored for %s", s.call.callID)
}

func (s *server) onInfo(m *sipMsg, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.call == nil || m.get("call-id") != s.call.callID {
		s.reply(addr, m, 481, "Call/Transaction Does Not Exist", newTag(), "", "")
		return
	}
	s.reply(addr, m, 200, "OK", s.call.toTag, "", "")
	if d, ok := infoDigit(m); ok {
		s.onDigit(s.call, d, "info")
	}
}

func (s *server) onOptions(m *sipMsg, addr *net.UDPAddr) {
	s.reply(addr, m, 200, "OK", newTag(),
		"Allow: INVITE, ACK, BYE, CANCEL, INFO, OPTIONS\r\nAccept: application/sdp\r\n", "")
}

func (s *server) onResponse(m *sipMsg) {
	_, method := parseCSeq(m.get("cseq"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.call != nil && s.call.state == stByeSent && m.get("call-id") == s.call.callID && strings.EqualFold(method, "BYE") {
		log.Printf("BYE completed (%d)", m.status)
		s.call = nil
	}
}

func (s *server) handleRTP(pkt []byte, addr *net.UDPAddr) {
	if len(pkt) < 12 || pkt[0]>>6 != 2 {
		return
	}
	pt := int(pkt[1] & 0x7f)
	if pt >= 72 && pt <= 76 {
		return
	}
	cc := int(pkt[0] & 0x0f)
	hdr := 12 + cc*4
	if pkt[0]&0x10 != 0 {
		if len(pkt) < hdr+4 {
			return
		}
		hdr += 4 + int(binary.BigEndian.Uint16(pkt[hdr+2:]))*4
	}
	if len(pkt) < hdr {
		return
	}
	payload := pkt[hdr:]
	if pkt[0]&0x20 != 0 && len(payload) > 0 {
		pad := int(payload[len(payload)-1])
		if pad > 0 && pad <= len(payload) {
			payload = payload[:len(payload)-pad]
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.call
	if c == nil || (c.state != stUp && c.state != stWaitAck) {
		return
	}
	if !c.latched {
		log.Printf("RTP from %s", addr)
		c.latched = true
	}
	c.rtpDest = addr
	if c.state != stUp {
		return
	}
	if c.dtmfPT >= 0 && pt == c.dtmfPT {
		s.rfc2833(c, payload)
		return
	}
	if pt != c.audioPT || len(payload) == 0 {
		return
	}
	pcm := make([]int16, len(payload))
	for i, b := range payload {
		if c.alaw {
			pcm[i] = alawDecode(b)
		} else {
			pcm[i] = mulawDecode(b)
		}
	}
	s.feedDTMF(c, pcm)
}

func (s *server) rfc2833(c *call, p []byte) {
	if len(p) < 4 {
		return
	}
	event := int(p[0])
	end := p[1]&0x80 != 0
	dur := int(p[2])<<8 | int(p[3])
	if event > 15 {
		return
	}
	digit := event
	if event > 9 {
		digit = -1
	}
	isNew := !c.dtmf.evOn || event != c.dtmf.evID || dur < c.dtmf.evDur
	if isNew {
		c.dtmf.evOn = true
		c.dtmf.evID = event
		c.dtmf.evTake = false
	}
	c.dtmf.evDur = dur
	if !c.dtmf.evTake && (end || dur >= 480) {
		c.dtmf.evTake = true
		s.onDigit(c, digit, "rfc2833")
	}
}

func (s *server) feedDTMF(c *call, pcm []int16) {
	c.dtmf.buf = append(c.dtmf.buf, pcm...)
	for len(c.dtmf.buf) >= 205 {
		d := decodeDTMF(c.dtmf.buf[:205])
		s.noteDTMF(c, d)
		if len(c.dtmf.buf) >= 80 {
			c.dtmf.buf = append([]int16(nil), c.dtmf.buf[80:]...)
		} else {
			c.dtmf.buf = nil
		}
	}
}

func (s *server) noteDTMF(c *call, d int) {
	if d >= 0 && d == c.dtmf.digit {
		c.dtmf.run++
	} else {
		c.dtmf.digit = d
		if d >= 0 {
			c.dtmf.run = 1
		} else {
			c.dtmf.run = 0
		}
	}
	if d < 0 {
		c.dtmf.gap++
		if c.dtmf.gap >= 3 {
			c.dtmf.armed = true
		}
		return
	}
	c.dtmf.gap = 0
	if c.dtmf.armed && c.dtmf.run >= 4 {
		c.dtmf.armed = false
		s.onDigit(c, d, "inband")
	}
}

func (s *server) advertiseFor(src *net.UDPAddr) string {
	if s.advertise != "" {
		return s.advertise
	}
	if src != nil && src.IP != nil && src.IP.IsLoopback() {
		return "127.0.0.1"
	}
	if s.routeIP != "" {
		return s.routeIP
	}
	return "127.0.0.1"
}

func (s *server) reply(addr *net.UDPAddr, m *sipMsg, code int, reason, toTag, extra, body string) []byte {
	msg := formatResponse(m, code, reason, toTag, extra, body)
	if addr != nil && s.sipConn != nil {
		_, _ = s.sipConn.WriteToUDP(msg, addr)
	}
	return msg
}

func formatResponse(m *sipMsg, code int, reason, toTag, extra, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", code, reason)
	for _, v := range m.all("via") {
		fmt.Fprintf(&b, "Via: %s\r\n", v)
	}
	fmt.Fprintf(&b, "From: %s\r\n", m.get("from"))
	to := m.get("to")
	if toTag != "" && !hasTag(to) {
		fmt.Fprintf(&b, "To: %s;tag=%s\r\n", to, toTag)
	} else {
		fmt.Fprintf(&b, "To: %s\r\n", to)
	}
	fmt.Fprintf(&b, "Call-ID: %s\r\n", m.get("call-id"))
	fmt.Fprintf(&b, "CSeq: %s\r\n", m.get("cseq"))
	b.WriteString("Server: Yggdrasil-IVR/1.0\r\n")
	b.WriteString(extra)
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return []byte(b.String())
}

type sipMsg struct {
	isResp bool
	status int
	reason string
	method string
	uri    string
	hdrs   []sipHdr
	body   string
}

type sipHdr struct {
	name  string
	value string
}

func (m *sipMsg) get(name string) string {
	name = strings.ToLower(name)
	for _, h := range m.hdrs {
		if h.name == name {
			return h.value
		}
	}
	return ""
}

func (m *sipMsg) all(name string) []string {
	name = strings.ToLower(name)
	var out []string
	for _, h := range m.hdrs {
		if h.name == name {
			out = append(out, h.value)
		}
	}
	return out
}

var compactHeader = map[string]string{
	"v": "via",
	"f": "from",
	"t": "to",
	"i": "call-id",
	"m": "contact",
	"c": "content-type",
	"l": "content-length",
	"e": "content-encoding",
	"k": "supported",
}

func parseSIP(pkt []byte) (*sipMsg, error) {
	text := string(pkt)
	hs := text
	body := ""
	if i := strings.Index(text, "\r\n\r\n"); i >= 0 {
		hs = text[:i]
		body = text[i+4:]
	} else if i := strings.Index(text, "\n\n"); i >= 0 {
		hs = text[:i]
		body = text[i+2:]
	}
	lines := strings.Split(strings.ReplaceAll(hs, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("empty message")
	}
	m := &sipMsg{}
	start := strings.Fields(strings.TrimSpace(lines[0]))
	if len(start) < 2 {
		return nil, fmt.Errorf("bad start line")
	}
	if strings.HasPrefix(start[0], "SIP/") {
		m.isResp = true
		code, err := strconv.Atoi(start[1])
		if err != nil {
			return nil, fmt.Errorf("bad status")
		}
		m.status = code
		m.reason = strings.Join(start[2:], " ")
	} else {
		if len(start) < 3 || !strings.HasPrefix(start[2], "SIP/") {
			return nil, fmt.Errorf("bad request line")
		}
		m.method = start[0]
		m.uri = start[1]
	}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if n := len(m.hdrs); n > 0 {
				m.hdrs[n-1].value += " " + strings.TrimSpace(line)
			}
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		if full, ok := compactHeader[name]; ok {
			name = full
		}
		m.hdrs = append(m.hdrs, sipHdr{name: name, value: strings.TrimSpace(line[colon+1:])})
	}
	if cl := m.get("content-length"); cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err == nil && n >= 0 && n < len(body) {
			body = body[:n]
		}
	}
	m.body = body
	return m, nil
}

type sdpOffer struct {
	ip     string
	port   int
	dtmfPT int
	pcmuPT int
	pcmaPT int
}

func parseSDP(body string) (sdpOffer, error) {
	o := sdpOffer{dtmfPT: -1, pcmuPT: -1, pcmaPT: -1}
	kind := map[int]string{}
	var offered []int
	haveAudio := false
	ignore := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		switch line[0] {
		case 'c':
			if ignore {
				continue
			}
			f := strings.Fields(line[2:])
			if len(f) >= 3 && strings.EqualFold(f[1], "IP4") {
				addr := f[2]
				if i := strings.IndexByte(addr, '/'); i >= 0 {
					addr = addr[:i]
				}
				o.ip = addr
			}
		case 'm':
			f := strings.Fields(line[2:])
			if len(f) < 3 || !strings.EqualFold(f[0], "audio") || !strings.Contains(strings.ToUpper(f[2]), "RTP/AVP") {
				if haveAudio {
					ignore = true
				}
				continue
			}
			if haveAudio {
				ignore = true
				continue
			}
			port, err := strconv.Atoi(f[1])
			if err != nil {
				return o, fmt.Errorf("bad m= port")
			}
			o.port = port
			haveAudio = true
			for _, tok := range f[3:] {
				pt, err := strconv.Atoi(tok)
				if err != nil || pt < 0 || pt > 127 {
					continue
				}
				offered = append(offered, pt)
				switch pt {
				case 0:
					kind[0] = "PCMU"
				case 8:
					kind[8] = "PCMA"
				}
			}
		case 'a':
			if ignore || !strings.HasPrefix(line, "a=rtpmap:") {
				continue
			}
			rest := strings.TrimSpace(line[len("a=rtpmap:"):])
			sp := strings.IndexByte(rest, ' ')
			if sp <= 0 {
				continue
			}
			pt, err := strconv.Atoi(rest[:sp])
			if err != nil || pt < 0 || pt > 127 {
				continue
			}
			name := rest[sp+1:]
			if i := strings.IndexByte(name, '/'); i >= 0 {
				name = name[:i]
			}
			kind[pt] = strings.ToUpper(name)
		}
	}
	if !haveAudio {
		return o, fmt.Errorf("no audio m-line")
	}
	for _, pt := range offered {
		switch kind[pt] {
		case "PCMU":
			if o.pcmuPT < 0 {
				o.pcmuPT = pt
			}
		case "PCMA":
			if o.pcmaPT < 0 {
				o.pcmaPT = pt
			}
		case "TELEPHONE-EVENT":
			if o.dtmfPT < 0 {
				o.dtmfPT = pt
			}
		}
	}
	return o, nil
}

func pickCodec(o sdpOffer) (pt int, alaw, ok bool) {
	if o.pcmuPT >= 0 {
		return o.pcmuPT, false, true
	}
	if o.pcmaPT >= 0 {
		return o.pcmaPT, true, true
	}
	return 0, false, false
}

func infoDigit(m *sipMsg) (int, bool) {
	body := m.body
	lower := strings.ToLower(body)
	if i := strings.Index(lower, "signal="); i >= 0 {
		rest := strings.TrimSpace(body[i+len("signal="):])
		if rest == "" {
			return 0, false
		}
		if rest[0] >= '0' && rest[0] <= '9' {
			return int(rest[0] - '0'), true
		}
		return -1, true
	}
	trim := strings.TrimSpace(body)
	if len(trim) == 1 && trim[0] >= '0' && trim[0] <= '9' {
		return int(trim[0] - '0'), true
	}
	return 0, false
}

func parseCSeq(v string) (int, string) {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0, ""
	}
	n, _ := strconv.Atoi(f[0])
	method := ""
	if len(f) > 1 {
		method = f[1]
	}
	return n, method
}

func hasTag(v string) bool {
	return strings.Contains(strings.ToLower(v), "tag=")
}

func sipURI(header string) string {
	i := strings.Index(strings.ToLower(header), "sip:")
	if i < 0 {
		return ""
	}
	s := header[i:]
	end := len(s)
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '>', ';', ' ', '\t', '\r', '\n':
			end = j
			return s[:end]
		}
	}
	return s[:end]
}

func routeIPv4() string {
	c, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	a, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || a.IP == nil {
		return ""
	}
	if v4 := a.IP.To4(); v4 != nil {
		return v4.String()
	}
	return a.IP.String()
}

func ip4(s string) net.IP {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return nil
	}
	return ip.To4()
}

func cloneAddr(a *net.UDPAddr) *net.UDPAddr {
	if a == nil {
		return nil
	}
	ip := make(net.IP, len(a.IP))
	copy(ip, a.IP)
	return &net.UDPAddr{IP: ip, Port: a.Port, Zone: a.Zone}
}

func newTag() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.BigEndian.PutUint32(b[:], uint32(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}

func randU32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}
