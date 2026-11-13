// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

const allowedMethods = "INVITE, ACK, BYE, CANCEL, OPTIONS, INFO"

// Config is the simulator's settings.
type Config struct {
	// Addr is the SIP UDP address to bind, host:port. It is bound as given;
	// nothing here assumes loopback.
	Addr string
	// Advertise is the IP put in Contact and in the SDP. Empty means the
	// bound host, or, for a wildcard bind, the local address the route to the
	// caller uses.
	Advertise string
	// RTPPortMin and RTPPortMax bound the media ports. Zero lets the system
	// choose.
	RTPPortMin, RTPPortMax int

	// CalledPrefix is the string the trunk's gateway puts in front of the
	// user part of the Request-URI (the gateway parameter destination-prefix).
	// When set, an INVITE's user must start with it; the prefix is removed
	// before the number keys the scenario, so DialogRecord.Number stays the
	// number that was dialled, while RequestURI keeps the wire form. An INVITE
	// whose user lacks the prefix is not expected: a gateway that lost the
	// prefix fails the run. Empty means no prefix is expected or removed.
	CalledPrefix string

	Scenario *Scenario
	Log      *slog.Logger
}

// Server is the simulator.
type Server struct {
	cfg  Config
	log  *slog.Logger
	conn *net.UDPConn
	wav  map[string][]int16
	// startedAt is the zero of the monotonic timeline every afterStartMs is
	// read on. It keeps its monotonic reading; see dialog.inviteAt.
	startedAt time.Time

	mu       sync.Mutex
	dialogs  map[string]*dialog
	order    []*dialog
	attempts map[string]int
	wg       sync.WaitGroup
}

// New binds the SIP socket and loads the audio the scenario names.
func New(cfg Config) (*Server, error) {
	if cfg.Scenario == nil {
		return nil, errors.New("calleesim: no scenario")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{
		cfg:       cfg,
		log:       cfg.Log,
		wav:       map[string][]int16{},
		startedAt: time.Now(),
		dialogs:   map[string]*dialog{},
		attempts:  map[string]int{},
	}
	for number, behaviours := range cfg.Scenario.Numbers {
		for i, b := range behaviours {
			if b.Uplink.Kind != UplinkWAV {
				continue
			}
			samples, err := readWAV(b.Uplink.File)
			if err != nil {
				return nil, fmt.Errorf("number %s attempt %d: %s: %w", number, i, b.Uplink.File, err)
			}
			s.wav[b.Uplink.File] = samples
		}
	}
	addr, err := net.ResolveUDPAddr("udp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", cfg.Addr, err)
	}
	if s.conn, err = net.ListenUDP("udp", addr); err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	return s, nil
}

// Addr is the bound SIP address.
func (s *Server) Addr() *net.UDPAddr { return s.conn.LocalAddr().(*net.UDPAddr) }

// Serve answers calls until ctx ends, then ends every dialog still open and
// returns. The report is complete once it has returned.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.conn.Close()
	}()
	buf := make([]byte, 65536)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		s.handle(append([]byte(nil), buf[:n]...), from)
	}
	s.mu.Lock()
	open := append([]*dialog(nil), s.order...)
	s.mu.Unlock()
	for _, d := range open {
		d.shutdown()
	}
	s.wg.Wait()
	if ctx.Err() == nil {
		return errors.New("calleesim: sip socket closed")
	}
	return nil
}

// Report snapshots every dialog seen so far.
func (s *Server) Report() Report {
	s.mu.Lock()
	// Taken under the lock onInvite stamps and registers a dialog under, so
	// every INVITE stamped before this moment is in the list copied below.
	takenMs := s.sinceStartMs(time.Now())
	dialogs := append([]*dialog(nil), s.order...)
	s.mu.Unlock()

	report := Report{StartedAt: s.startedAt.UTC(), AfterStartMs: takenMs, Dialogs: make([]DialogRecord, 0, len(dialogs))}
	for _, d := range dialogs {
		d.mu.Lock()
		rec := d.rec.snapshot()
		d.mu.Unlock()
		if !rec.IsExpected {
			report.UnexpectedCount++
		}
		report.Dialogs = append(report.Dialogs, rec)
	}
	report.Failed = report.UnexpectedCount > 0
	return report
}

func (s *Server) handle(data []byte, from *net.UDPAddr) {
	msg, err := voice.ParseSIP(data)
	if err != nil || msg.StatusCode() != 0 {
		return // keep-alives, garbage and responses to our own BYE
	}
	switch msg.Method() {
	case "INVITE":
		s.onInvite(msg, from)
	case "ACK", "BYE", "CANCEL":
		s.mu.Lock()
		d := s.dialogs[msg.CallID()]
		s.mu.Unlock()
		if d == nil {
			if msg.Method() != "ACK" {
				s.send(from, msg.Response("481 Call/Transaction Does Not Exist", "", "", "", nil))
			}
			return
		}
		switch msg.Method() {
		case "ACK":
			d.onAck()
		case "BYE":
			d.onBye(msg)
		case "CANCEL":
			d.onCancel(msg)
		}
	case "OPTIONS":
		s.send(from, msg.Response("200 OK", "", "", "Allow: "+allowedMethods+"\r\n", nil))
	case "INFO":
		s.send(from, msg.Response("200 OK", "", "", "", nil))
	default:
		s.send(from, msg.Response("405 Method Not Allowed", "", "", "Allow: "+allowedMethods+"\r\n", nil))
	}
}

func (s *Server) send(to *net.UDPAddr, packet []byte) {
	if _, err := s.conn.WriteToUDP(packet, to); err != nil {
		s.log.Warn("sip send failed", "to", to.String(), "error", err)
	}
}

func (s *Server) onInvite(msg *voice.SIPMessage, from *net.UDPAddr) {
	s.mu.Lock()
	if existing := s.dialogs[msg.CallID()]; existing != nil {
		s.mu.Unlock()
		existing.retransmit() // the caller did not see our last response
		return
	}
	number, prefixed := stripPrefix(numberOf(msg.RequestURI()), s.cfg.CalledPrefix)
	attempt := s.attempts[number]
	s.attempts[number]++
	behaviour, known := s.cfg.Scenario.For(number, attempt)
	if !prefixed {
		known = false
	}

	ctx, cancel := context.WithCancel(context.Background())
	inviteAt := time.Now() // keeps its monotonic reading; see dialog.inviteAt
	d := &dialog{
		s: s, invite: msg, inviteAt: inviteAt, from: from, behaviour: behaviour, ctx: ctx, cancel: cancel,
		toTag: randomHex(4),
		ackCh: make(chan struct{}),
		rec: DialogRecord{
			CallID: msg.CallID(), Number: number, Attempt: attempt, IsExpected: known,
			RemoteAddr: from.String(), InviteAt: inviteAt.UTC(), InviteAfterStartMs: s.sinceStartMs(inviteAt),
			RequestURI: msg.RequestURI(), InviteHeaders: msg.Headers(), Provisionals: []ProvisionalRecord{},
			Downlink: Downlink{Energy: []EnergyBucket{}},
		},
	}
	s.dialogs[msg.CallID()] = d
	s.order = append(s.order, d)
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		d.run(known)
	}()
}

// sinceStartMs places t, a reading that still carries its monotonic part, on
// the simulator's monotonic timeline.
func (s *Server) sinceStartMs(t time.Time) float64 {
	return monotonicMs(t, s.startedAt)
}

// monotonicMs is t minus from in milliseconds. Both must carry their monotonic
// readings (time.Now(), never a value passed through UTC, Round or JSON), or
// the subtraction falls back to the wall clock without a sound.
func monotonicMs(t, from time.Time) float64 {
	return float64(t.Sub(from)) / float64(time.Millisecond)
}

// stripPrefix removes the called-number prefix from a Request-URI user. With
// no prefix configured the user is returned as it is; with one, ok is false
// when the user does not start with it.
func stripPrefix(user, prefix string) (number string, ok bool) {
	if prefix == "" {
		return user, true
	}
	return strings.CutPrefix(user, prefix)
}

// numberOf is the user part of a SIP request URI.
func numberOf(requestURI string) string {
	_, rest, _ := strings.Cut(requestURI, ":")
	user, _, found := strings.Cut(rest, "@")
	if !found {
		return ""
	}
	user, _, _ = strings.Cut(user, ";")
	return user
}

// localIPFor is the address to advertise to a caller.
func (s *Server) localIPFor(peer *net.UDPAddr) string {
	if s.cfg.Advertise != "" {
		return s.cfg.Advertise
	}
	if ip := s.Addr().IP; ip != nil && !ip.IsUnspecified() {
		return ip.String()
	}
	if c, err := net.DialUDP("udp", nil, peer); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

// dialog is one INVITE being played out.
type dialog struct {
	s      *Server
	invite *voice.SIPMessage
	// inviteAt is the INVITE's arrival with its monotonic reading. Time.UTC()
	// strips that reading silently, so rec.InviteAt cannot serve: AfterInviteMs
	// is measured from this field, on the clock a wall-clock step cannot move.
	inviteAt  time.Time
	from      *net.UDPAddr
	behaviour Behaviour
	toTag     string
	ctx       context.Context
	cancel    context.CancelFunc

	ackCh   chan struct{}
	ackOnce sync.Once

	mu           sync.Mutex
	rec          DialogRecord
	ringing      bool // no final response sent yet
	ended        bool
	answeredAt   time.Time
	lastResponse []byte
	rtp          *voice.RTPSession
	law          media.Law
	localRTPPort int
	localIP      string
}

func (d *dialog) contactHeader() string {
	return fmt.Sprintf("Contact: <sip:calleesim@%s:%d>\r\n", d.localIP, d.s.Addr().Port)
}

// run plays the behaviour: provisionals, the final response, an optional BYE.
func (d *dialog) run(known bool) {
	d.mu.Lock()
	d.ringing = true
	d.localIP = d.s.localIPFor(d.from)
	d.mu.Unlock()
	d.s.send(d.from, d.invite.Response("100 Trying", "", "", "", nil))

	if !known {
		d.finalResponse(404, "404 Not Found")
		return
	}
	offer := voice.ParseSDP(d.invite.Body())
	law, payloadType, ok := voice.NegotiateCodec(offer, []media.Law{media.LawMu, media.LawAlaw})
	if !ok {
		d.finalResponse(488, "488 Not Acceptable Here")
		return
	}
	d.law = law
	d.mu.Lock()
	d.rec.Codec = law.String()
	d.mu.Unlock()

	start := d.rec.InviteAt
	for _, p := range d.behaviour.sortedProvisionals() {
		if !d.sleepUntil(start.Add(ms(p.AtMs))) {
			return
		}
		d.provisional(p, offer, payloadType)
	}
	if !d.sleepUntil(start.Add(ms(d.behaviour.FinalAfterMs))) {
		return
	}

	status := d.behaviour.Final()
	if status != 200 {
		d.finalResponse(status, fmt.Sprintf("%d %s", status, reasonFor(status)))
		return
	}
	d.answer(offer, payloadType)
}

// sleepUntil waits for t; false when the dialog ended first.
func (d *dialog) sleepUntil(t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-d.ctx.Done():
		return false
	}
}

func (d *dialog) provisional(p Provisional, offer *voice.SDPOffer, payloadType int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ringing {
		return // cancelled while the timer fired
	}
	extra := d.contactHeader()
	var body []byte
	if p.EarlyMedia {
		if err := d.startMediaLocked(offer, payloadType); err != nil {
			d.s.log.Warn("early media unavailable", "sipCallId", d.rec.CallID, "error", err)
		} else {
			extra += "Content-Type: application/sdp\r\n"
			body = []byte(d.sdpLocked(payloadType, offer))
		}
	}
	packet := d.invite.Response(fmt.Sprintf("%d %s", p.Status, reasonFor(p.Status)), d.toTag, "", extra, body)
	d.lastResponse = packet
	d.rec.Provisionals = append(d.rec.Provisionals, ProvisionalRecord{
		Status: p.Status, SentAt: time.Now().UTC(), HasEarlyMedia: len(body) > 0,
	})
	d.s.send(d.from, packet)
}

// finalResponse sends a non-2xx final response and ends the dialog's media.
func (d *dialog) finalResponse(status int, line string) {
	d.mu.Lock()
	if !d.ringing {
		d.mu.Unlock()
		return
	}
	d.ringing = false
	packet := d.invite.Response(line, d.toTag, "", "", nil)
	d.lastResponse = packet
	d.rec.Final = d.finalRecord(status, time.Now())
	d.s.send(d.from, packet)
	d.mu.Unlock()
	d.stopMedia()
}

// answer sends the 200 OK, keeps retransmitting it until the ACK, and hangs up
// when the behaviour says so.
func (d *dialog) answer(offer *voice.SDPOffer, payloadType int) {
	d.mu.Lock()
	if !d.ringing {
		d.mu.Unlock()
		return
	}
	if err := d.startMediaLocked(offer, payloadType); err != nil {
		d.mu.Unlock()
		d.s.log.Warn("media unavailable", "sipCallId", d.rec.CallID, "error", err)
		d.finalResponse(500, "500 Server Internal Error")
		return
	}
	d.ringing = false
	packet := d.invite.Response("200 OK", d.toTag, "",
		d.contactHeader()+"Allow: "+allowedMethods+"\r\nContent-Type: application/sdp\r\n",
		[]byte(d.sdpLocked(payloadType, offer)))
	d.lastResponse = packet
	d.answeredAt = time.Now()
	d.rec.Final = d.finalRecord(200, d.answeredAt)
	d.s.send(d.from, packet)
	d.mu.Unlock()

	go d.retransmitUntilAck()

	if d.behaviour.ByeAfterMs != nil {
		if !d.sleepUntil(d.answeredAt.Add(ms(*d.behaviour.ByeAfterMs))) {
			return
		}
		d.hangUp()
	}
}

// retransmitUntilAck repeats the 200 OK as RFC 3261 §13.3.1.4 asks, since UDP
// may have lost it.
func (d *dialog) retransmitUntilAck() {
	delay := 500 * time.Millisecond
	for total := time.Duration(0); total < 32*time.Second; total += delay {
		select {
		case <-d.ackCh:
			return
		case <-d.ctx.Done():
			return
		case <-time.After(delay):
		}
		d.retransmit()
		delay = min(delay*2, 4*time.Second)
	}
}

func (d *dialog) retransmit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastResponse != nil {
		d.s.send(d.from, d.lastResponse)
	}
}

// hangUp sends the callee's BYE.
func (d *dialog) hangUp() {
	d.mu.Lock()
	if d.ended {
		d.mu.Unlock()
		return
	}
	d.ended = true
	target := d.byeTarget()
	// A UAS's route set is the INVITE's Record-Route values in the order
	// received (RFC 3261 §12.1.1). BuildBye writes them as Route headers in that
	// order, and byeTarget sends the BYE to the first of them.
	packet := voice.BuildBye(d.rec.CallID, d.invite.From(), d.invite.To(), d.toTag,
		d.localIP, d.s.Addr().Port, d.invite.RecordRoutes(), d.invite.Contact())
	d.rec.End = d.endRecord(EndedWithBye, EndedByCallee, time.Now())
	d.s.send(target, packet)
	d.mu.Unlock()
	d.finish()
}

// byeTarget is where the callee's BYE goes, by the rule the bot's own BYE
// follows: the top of the route set when the INVITE was record-routed, so
// every proxy that asked to stay in the dialog sees it, and otherwise the
// caller's Contact. The address the INVITE came from is the fallback for an
// entry or a Contact that is not an IP address reachable over UDP.
func (d *dialog) byeTarget() *net.UDPAddr {
	if routeSet := d.invite.RecordRoutes(); len(routeSet) > 0 {
		addr, err := voice.RouteSetNextHop(routeSet)
		if err != nil {
			d.s.log.Warn("bye cannot go to the top of its route set, sending it to the invite's source",
				"sipCallId", d.invite.CallID(), "route", routeSet[0], "error", err)
			return d.from
		}
		return addr
	}
	if contact := d.invite.Contact(); contact != "" {
		if addr, err := voice.UDPAddrOfURI(voice.URIOfHeader(contact)); err == nil {
			return addr
		}
	}
	return d.from
}

func (d *dialog) onAck() {
	d.mu.Lock()
	if d.rec.AckAt == nil && d.rec.Final != nil {
		at := time.Now().UTC()
		d.rec.AckAt = &at
	}
	d.mu.Unlock()
	d.ackOnce.Do(func() { close(d.ackCh) })
}

func (d *dialog) onBye(bye *voice.SIPMessage) {
	d.mu.Lock()
	d.s.send(d.from, bye.Response("200 OK", d.toTag, "", "", nil))
	if d.ended {
		d.mu.Unlock()
		return
	}
	d.ended = true
	d.rec.End = d.endRecord(EndedWithBye, EndedByCaller, time.Now())
	d.mu.Unlock()
	d.finish()
}

// onCancel answers 487 to an INVITE still ringing; a CANCEL that comes after
// the final response changes nothing and only gets its own 200.
func (d *dialog) onCancel(cancel *voice.SIPMessage) {
	d.mu.Lock()
	d.s.send(d.from, cancel.Response("200 OK", d.toTag, "", "", nil))
	if !d.ringing {
		d.mu.Unlock()
		return
	}
	d.ringing, d.ended = false, true
	now := time.Now()
	packet := d.invite.Response487(d.toTag)
	d.lastResponse = packet
	d.rec.Final = d.finalRecord(487, now)
	d.rec.End = d.endRecord(EndedWithCancel, EndedByCaller, now)
	d.s.send(d.from, packet)
	d.mu.Unlock()
	d.finish()
}

// finalRecord is the record of the final response sent at now (a reading
// that still carries its monotonic part). Callers hold d.mu.
func (d *dialog) finalRecord(status int, now time.Time) *FinalRecord {
	return &FinalRecord{
		Status: status, SentAt: now.UTC(),
		AfterInviteMs: monotonicMs(now, d.inviteAt), AfterStartMs: d.s.sinceStartMs(now),
	}
}

// endRecord is the record of the request that ended the dialog, which arrived
// at now (a reading that still carries its monotonic part). Callers hold d.mu.
func (d *dialog) endRecord(method, by string, now time.Time) *EndRecord {
	return &EndRecord{
		Method: method, By: by, At: now.UTC(),
		AfterInviteMs: monotonicMs(now, d.inviteAt), AfterStartMs: d.s.sinceStartMs(now),
	}
}

// shutdown ends a dialog the process is abandoning. It records nothing: the
// report says what happened on the wire, and nothing happened.
func (d *dialog) shutdown() { d.finish() }

func (d *dialog) finish() {
	d.cancel()
	d.stopMedia()
}

func (d *dialog) stopMedia() {
	d.mu.Lock()
	session := d.rtp
	d.mu.Unlock()
	if session != nil {
		session.Stop()
	}
}

func reasonFor(status int) string {
	switch status {
	case 180:
		return "Ringing"
	case 183:
		return "Session Progress"
	case 486:
		return "Busy Here"
	case 487:
		return "Request Terminated"
	case 600:
		return "Busy Everywhere"
	case 603:
		return "Decline"
	case 604:
		return "Does Not Exist Anywhere"
	}
	if text := http.StatusText(status); text != "" {
		return text
	}
	return "Status"
}

// Handler serves the report as JSON: GET /report, or GET /dialogs for the
// dialogs alone.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.Report())
	})
	mux.HandleFunc("GET /dialogs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.Report().Dialogs)
	})
	return mux
}

// randomHex returns n random bytes as 2n hex characters. SIP tokens (Call-ID,
// tags, branches) are random, not ids: a UUIDv7's leading hex is a timestamp.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
