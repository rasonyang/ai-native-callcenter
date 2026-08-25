// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// Config parameterises the user agent server.
type Config struct {
	// SIPHost and SIPPort are where INVITEs are accepted. This is the target
	// of the switch's bot gateway.
	SIPHost string
	SIPPort int
	// AdvertiseIP is the address written into SDP answers. Empty means probe
	// the route towards the peer's media address, which is the only thing that
	// works when the listener is bound to every interface.
	AdvertiseIP string
	// RTPPortRange is inclusive. Each call takes an even port for RTP and the
	// odd one above it for RTCP, so the usable capacity is half the range.
	RTPPortRange [2]int
	// CodecPreferences is the local preference order among offered codecs.
	CodecPreferences []media.Law
	// RTPDeadTimeout ends a call whose media has stopped arriving even though
	// the dialog is nominally up. Zero disables the check.
	RTPDeadTimeout time.Duration
	// AckTimeout is how long an accepted INVITE waits for its ACK.
	AckTimeout time.Duration
	// MaxCalls is the admission limit; further INVITEs get 486 Busy Here.
	MaxCalls int
	// IsDTMFEnabled controls whether telephone-event is answered in SDP and
	// digits are surfaced at all.
	IsDTMFEnabled bool
	// RTCPInterval is the reporting period. Zero disables RTCP.
	RTCPInterval time.Duration
	Logger       *slog.Logger
}

// DefaultConfig returns the settings this deployment runs with.
func DefaultConfig() Config {
	return Config{
		SIPHost: "0.0.0.0",
		SIPPort: 6060,
		// Deliberately clear of the switch's own 16384-32768 range, since a
		// development host runs both. Five hundred pairs covers the concurrent
		// call target with room to spare.
		RTPPortRange:     [2]int{40000, 40999},
		CodecPreferences: []media.Law{media.LawMu, media.LawAlaw},
		RTPDeadTimeout:   5 * time.Second,
		AckTimeout:       3 * time.Second,
		MaxCalls:         220,
		IsDTMFEnabled:    true,
		RTCPInterval:     DefaultRTCPInterval,
	}
}

// Dialog is one accepted call: the SIP state needed to end it properly, plus
// its media session.
type Dialog struct {
	CallID    string
	LocalTag  string
	RemoteTag string

	FromHeader    string
	ToHeader      string
	CSeq          string
	RemoteContact string
	RecordRoutes  []string
	// CustomHeaders carries the X-* headers from the INVITE, which is how the
	// call is correlated with its leg on the switch.
	CustomHeaders map[string]string

	RemoteRTPAddr *net.UDPAddr
	LocalRTPPort  int
	LocalIP       string
	LocalSIPPort  int

	RTP *RTPSession

	// Stopped closes when the dialog ends, whichever side ended it.
	Stopped chan struct{}

	rtcpInterval time.Duration
	log          *slog.Logger
	// invite is retained so a CANCEL can be answered inside the INVITE's own
	// transaction, with its Via set and its CSeq.
	invite *sipMessage

	mu sync.Mutex
	// answer is the 200 OK, kept so a retransmitted INVITE can be answered
	// with the identical response instead of a second, conflicting one.
	answer []byte
	// rtcp is nil when reporting is disabled or its port could not be bound.
	// It is created after the dialog is already visible to other goroutines,
	// so it lives under the mutex.
	rtcp      *RTCPSession
	isStopped bool
	isByeSent bool
	// byeReason rides the BYE this dialog sends, when there is something to
	// say beyond the call being over. Zero means an ordinary goodbye.
	byeReason   byeReason
	isAcked     bool
	isRemoteBye bool
	sipConn     *net.UDPConn
	sipAddr     *net.UDPAddr
}

func (d *Dialog) setSIPTransport(conn *net.UDPConn, addr *net.UDPAddr) {
	d.mu.Lock()
	d.sipConn, d.sipAddr = conn, addr
	d.mu.Unlock()
}

func (d *Dialog) startMedia() error {
	if err := d.RTP.Start(d.RemoteRTPAddr); err != nil {
		return err
	}
	if d.rtcpInterval <= 0 {
		return nil
	}
	// RTCP is observability, not the call. Losing it — a busy port, say —
	// must not take the audio down with it.
	session, err := NewRTCPSession(d.RTP, d.RemoteRTPAddr, d.rtcpInterval, d.log)
	if err != nil {
		d.log.Warn("rtcp disabled for call", "callId", d.CallID, "error", err)
		return nil
	}
	d.mu.Lock()
	// The call can be torn down while this is being set up, in which case Stop
	// has already run and would never see this session.
	if d.isStopped {
		d.mu.Unlock()
		session.Stop()
		return nil
	}
	d.rtcp = session
	d.mu.Unlock()

	session.Start()
	return nil
}

// Quality reports what the peer says about the audio we send it, which is the
// only way to notice that our audio is not arriving. The zero value means RTCP
// is off or the peer has not reported yet.
func (d *Dialog) Quality() RemoteQuality {
	d.mu.Lock()
	session := d.rtcp
	d.mu.Unlock()
	if session == nil {
		return RemoteQuality{}
	}
	return session.Quality()
}

// sendBye ends the dialog from this side. It is a no-op once the peer has sent
// its own BYE: answering a BYE with a BYE is a protocol error that some proxies
// respond to by tearing down the wrong thing.
func (d *Dialog) sendBye() {
	d.mu.Lock()
	if d.isByeSent || d.isRemoteBye {
		d.mu.Unlock()
		return
	}
	d.isByeSent = true
	conn, addr, reason := d.sipConn, d.sipAddr, d.byeReason
	d.mu.Unlock()

	if conn == nil || addr == nil {
		return
	}
	bye := buildBye(d.CallID, d.FromHeader, d.ToHeader, d.LocalTag,
		d.LocalIP, d.LocalSIPPort, d.RecordRoutes, d.RemoteContact, reason)
	if _, err := conn.WriteToUDP(bye, addr); err == nil {
		d.log.Info("sip bye sent", "callId", d.CallID,
			"sipCause", reason.SIPCause, "q850Cause", reason.Q850Cause)
	}
}

// StopWithReason ends the dialog and says why on the BYE.
//
// The reason has to be set before the BYE goes out and there is only one BYE,
// so this is Stop with the answer attached rather than a way to change it
// afterwards.
func (d *Dialog) StopWithReason(reason byeReason) {
	d.mu.Lock()
	if !d.isByeSent {
		d.byeReason = reason
	}
	d.mu.Unlock()
	d.Stop()
}

// Stop ends the call and releases its media. It is idempotent.
func (d *Dialog) Stop() {
	d.mu.Lock()
	if d.isStopped {
		d.mu.Unlock()
		return
	}
	d.isStopped = true
	rtcp := d.rtcp
	d.mu.Unlock()

	close(d.Stopped)
	d.sendBye()
	if rtcp != nil {
		rtcp.Stop()
	}
	d.RTP.Stop()
}

// IsStopped reports whether the dialog has ended.
func (d *Dialog) IsStopped() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isStopped
}

func (d *Dialog) markRemoteBye() {
	d.mu.Lock()
	d.isRemoteBye = true
	d.mu.Unlock()
}

// UAS accepts calls from the switch and hands each one to its callbacks, which
// run on their own goroutines.
type UAS struct {
	cfg Config
	log *slog.Logger

	// OnCallStarted fires once media is flowing. It owns the dialog from then
	// on and must not block.
	OnCallStarted func(*Dialog)
	// OnCallEnded fires exactly once per started call.
	OnCallEnded func(*Dialog)
	// OnCallFailed fires when an accepted call never got as far as media.
	OnCallFailed func(*Dialog, error)

	conn      *net.UDPConn
	localPort int

	mu          sync.Mutex
	isRunning   bool
	dialogs     map[string]*Dialog
	ackTimers   map[string]*time.Timer
	answerTimer map[string]*time.Timer
	usedPorts   map[int]bool
}

func NewUAS(cfg Config) *UAS {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if len(cfg.CodecPreferences) == 0 {
		cfg.CodecPreferences = DefaultConfig().CodecPreferences
	}
	return &UAS{
		cfg:         cfg,
		log:         cfg.Logger,
		dialogs:     map[string]*Dialog{},
		ackTimers:   map[string]*time.Timer{},
		answerTimer: map[string]*time.Timer{},
		usedPorts:   map[int]bool{},
	}
}

func (u *UAS) Start() error {
	ip := net.ParseIP(u.cfg.SIPHost)
	if ip == nil {
		ip = net.IPv4zero
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: u.cfg.SIPPort})
	if err != nil {
		return err
	}
	u.conn = conn
	u.localPort = conn.LocalAddr().(*net.UDPAddr).Port

	u.mu.Lock()
	u.isRunning = true
	u.mu.Unlock()

	go u.readLoop()
	u.log.Info("sip uas started", "host", u.cfg.SIPHost, "port", u.localPort,
		"maxCalls", u.cfg.MaxCalls)
	return nil
}

func (u *UAS) Stop() {
	u.mu.Lock()
	u.isRunning = false
	for _, timer := range u.ackTimers {
		timer.Stop()
	}
	for _, timer := range u.answerTimer {
		timer.Stop()
	}
	u.ackTimers = map[string]*time.Timer{}
	u.answerTimer = map[string]*time.Timer{}
	dialogs := make([]*Dialog, 0, len(u.dialogs))
	for _, d := range u.dialogs {
		dialogs = append(dialogs, d)
	}
	u.dialogs = map[string]*Dialog{}
	u.usedPorts = map[int]bool{}
	u.mu.Unlock()

	// Every conversation still running ends with a BYE that says why. The
	// caller is not hanging up and the bot has not finished with them: this
	// process is going away, and that is a different thing for the switch to
	// know. aicc_inbound.lua keeps such a caller alive and hands them to a
	// person; without the reason it cannot tell this from a bot that reached
	// its goodbye, and neither can the CDR.
	if len(dialogs) > 0 {
		slog.Warn("ending conversations in progress: this process is restarting",
			"calls", len(dialogs),
			"sipCause", byeReasonRestart.SIPCause, "q850Cause", byeReasonRestart.Q850Cause)
	}
	for _, d := range dialogs {
		d.StopWithReason(byeReasonRestart)
	}
	if u.conn != nil {
		_ = u.conn.Close()
	}
	u.log.Info("sip uas stopped")
}

// LocalPort is the bound SIP port, resolved after Start.
func (u *UAS) LocalPort() int { return u.localPort }

// ActiveCalls is the number of dialogs currently up.
func (u *UAS) ActiveCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.dialogs)
}

//
// Port pool.
//

// allocateRTPPort claims an even port and the odd one above it, so one call's
// RTCP can never land on another call's RTP.
func (u *UAS) allocateRTPPort() (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	low, high := u.cfg.RTPPortRange[0], u.cfg.RTPPortRange[1]
	if low%2 != 0 {
		low++
	}
	claim := func(port int) bool {
		if port < low || port+1 > high || u.usedPorts[port] || u.usedPorts[port+1] {
			return false
		}
		u.usedPorts[port], u.usedPorts[port+1] = true, true
		return true
	}

	// Random first so consecutive calls do not reuse a port pair that the
	// previous peer may still be sending to; a linear sweep as the fallback so
	// a nearly full pool still succeeds.
	for range 100 {
		if port := low + rand.IntN((high-low)/2+1)*2; claim(port) {
			return port, nil
		}
	}
	for port := low; port+1 <= high; port += 2 {
		if claim(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no RTP ports available in %d-%d", low, high)
}

func (u *UAS) releaseRTPPort(port int) {
	u.mu.Lock()
	delete(u.usedPorts, port)
	delete(u.usedPorts, port+1)
	u.mu.Unlock()
}

// advertiseIP decides what address to put in the SDP answer.
//
// The probe goes towards the peer's *media* address, not the source of its
// signalling: a switch may signal from the loopback while its media stack is
// bound to a LAN address, and answering with 127.0.0.1 produces a call that
// connects and then has no audio in either direction.
func (u *UAS) advertiseIP(peerIP string, peerPort int) string {
	if u.cfg.AdvertiseIP != "" {
		return u.cfg.AdvertiseIP
	}
	if bound := u.conn.LocalAddr().(*net.UDPAddr).IP.String(); bound != "0.0.0.0" && bound != "::" {
		return bound
	}
	if peerPort == 0 {
		peerPort = 5060
	}
	probe, err := net.Dial("udp4", net.JoinHostPort(peerIP, fmt.Sprint(peerPort)))
	if err != nil {
		return "127.0.0.1"
	}
	defer probe.Close()
	return probe.LocalAddr().(*net.UDPAddr).IP.String()
}

//
// Signalling.
//

func (u *UAS) readLoop() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		u.handleMessage(data, addr)
	}
}

func (u *UAS) handleMessage(data []byte, addr *net.UDPAddr) {
	msg, err := parseSIP(data)
	if err != nil {
		u.log.Error("sip parse failed", "from", addr.String(), "error", err)
		return
	}
	switch msg.method {
	case methodInvite:
		u.mu.Lock()
		isRunning := u.isRunning
		u.mu.Unlock()
		if isRunning {
			u.handleInvite(msg, addr)
		}
	case methodAck:
		u.handleAck(msg)
	case methodBye:
		u.handleBye(msg, addr)
	case methodCancel:
		u.handleCancel(msg, addr)
	case methodOptions:
		u.send(build200OKOptions(msg), addr)
	case methodInfo:
		u.handleInfo(msg, addr)
	default:
		u.log.Debug("sip method not handled", "method", string(msg.method))
	}
}

func (u *UAS) send(data []byte, addr *net.UDPAddr) {
	if u.conn != nil {
		_, _ = u.conn.WriteToUDP(data, addr)
	}
}

func (u *UAS) handleInvite(msg *sipMessage, addr *net.UDPAddr) {
	callID := msg.callID()

	u.mu.Lock()
	existing := u.dialogs[callID]
	callCount := len(u.dialogs)
	u.mu.Unlock()

	// A retransmitted INVITE means our 200 OK did not arrive. Answering with
	// the identical response is what the caller is waiting for; treating it as
	// a duplicate and dropping it leaves the call ringing until it times out.
	if existing != nil {
		existing.mu.Lock()
		answer := existing.answer
		existing.mu.Unlock()
		if answer != nil {
			u.send(answer, addr)
		}
		return
	}

	if callCount >= u.cfg.MaxCalls {
		u.send(buildReject(msg, "486 Busy Here", newTag()), addr)
		u.log.Warn("invite rejected at capacity", "callId", callID, "maxCalls", u.cfg.MaxCalls)
		return
	}

	u.send(build100Trying(msg), addr)

	offer := parseSDP(msg.body)
	if offer.IP == "" || offer.Port == 0 {
		u.log.Error("invite carried no usable sdp", "callId", callID)
		u.send(buildReject(msg, "488 Not Acceptable Here", ""), addr)
		return
	}
	law, payloadType, ok := negotiate(offer, u.cfg.CodecPreferences)
	if !ok {
		u.log.Error("no common codec", "callId", callID, "offered", offer.Codecs)
		u.send(buildReject(msg, "488 Not Acceptable Here", ""), addr)
		return
	}

	localRTPPort, err := u.allocateRTPPort()
	if err != nil {
		u.log.Error("no rtp port for call", "callId", callID, "error", err)
		u.send(buildReject(msg, "503 Service Unavailable", ""), addr)
		return
	}

	dtmfPayloadType := -1
	if u.cfg.IsDTMFEnabled {
		dtmfPayloadType = offer.DTMFPayloadType
	}

	localIP := u.advertiseIP(offer.IP, offer.Port)
	sessionID := rand.Uint32()
	localTag := fmt.Sprintf("aicc-%d", sessionID)

	dialog := &Dialog{
		CallID:        callID,
		LocalTag:      localTag,
		RemoteTag:     tagFrom(msg.fromHeader()),
		FromHeader:    msg.fromHeader(),
		ToHeader:      msg.toHeader(),
		CSeq:          msg.cseq(),
		RemoteContact: msg.contact(),
		RecordRoutes:  slices.Clone(msg.recordRoutes()),
		CustomHeaders: msg.customHeaders(),
		RemoteRTPAddr: &net.UDPAddr{IP: net.ParseIP(offer.IP), Port: offer.Port},
		LocalRTPPort:  localRTPPort,
		LocalIP:       localIP,
		LocalSIPPort:  u.localPort,
		RTP:           NewRTPSession(localRTPPort, law, dtmfPayloadType, u.log),
		Stopped:       make(chan struct{}),
		rtcpInterval:  u.cfg.RTCPInterval,
		log:           u.log,
		invite:        msg,
	}
	dialog.setSIPTransport(u.conn, addr)

	sdp := buildSDPAnswer(localIP, localRTPPort, sessionID, law, payloadType, dtmfPayloadType)
	answer := build200OKInvite(msg, sdp, localIP, u.localPort, localTag)
	dialog.mu.Lock()
	dialog.answer = answer
	dialog.mu.Unlock()

	u.mu.Lock()
	u.dialogs[callID] = dialog
	u.ackTimers[callID] = time.AfterFunc(u.cfg.AckTimeout, func() {
		u.log.Warn("no ack for accepted invite", "callId", callID)
		u.cleanup(callID)
	})
	u.mu.Unlock()

	u.send(answer, addr)
	u.retransmitAnswer(dialog, addr)

	u.log.Info("invite accepted", "callId", callID, "codec", law.String(),
		"remoteRtp", fmt.Sprintf("%s:%d", offer.IP, offer.Port),
		"advertiseIp", localIP, "rtpPort", localRTPPort,
		"dtmfPayloadType", dtmfPayloadType, "headers", dialog.CustomHeaders)
}

// retransmitAnswer re-sends the 200 OK until the ACK arrives.
//
// The 200 OK to an INVITE is the one response a UAS is responsible for
// retransmitting itself (RFC 3261 §13.3.1.4) — no transaction layer covers it.
// Without this, a single lost packet leaves the caller hearing silence on a
// call we believe is up. The interval doubles from 500 ms and caps at 4 s; the
// ACK timeout ends the attempt.
func (u *UAS) retransmitAnswer(dialog *Dialog, addr *net.UDPAddr) {
	interval := 500 * time.Millisecond
	const maxInterval = 4 * time.Second

	var schedule func()
	schedule = func() {
		u.mu.Lock()
		if !u.isRunning {
			u.mu.Unlock()
			return
		}
		u.answerTimer[dialog.CallID] = time.AfterFunc(interval, func() {
			dialog.mu.Lock()
			isAcked, isStopped, answer := dialog.isAcked, dialog.isStopped, dialog.answer
			dialog.mu.Unlock()
			if isAcked || isStopped {
				return
			}
			u.send(answer, addr)
			u.log.Debug("retransmitted 200 ok", "callId", dialog.CallID, "after", interval)
			interval = min(interval*2, maxInterval)
			schedule()
		})
		u.mu.Unlock()
	}
	schedule()
}

func (u *UAS) handleAck(msg *sipMessage) {
	callID := msg.callID()

	u.mu.Lock()
	if timer, ok := u.ackTimers[callID]; ok {
		timer.Stop()
		delete(u.ackTimers, callID)
	}
	if timer, ok := u.answerTimer[callID]; ok {
		timer.Stop()
		delete(u.answerTimer, callID)
	}
	dialog := u.dialogs[callID]
	u.mu.Unlock()

	if dialog == nil {
		return
	}
	dialog.mu.Lock()
	// The ACK is retransmitted alongside the 200 OK it acknowledges, so only
	// the first one starts the call.
	isFirst := !dialog.isAcked
	dialog.isAcked = true
	dialog.mu.Unlock()

	if isFirst {
		go u.startCall(dialog)
	}
}

// handleCancel withdraws a call that was accepted but never acknowledged.
func (u *UAS) handleCancel(msg *sipMessage, addr *net.UDPAddr) {
	u.send(build200OK(msg), addr)

	callID := msg.callID()
	u.mu.Lock()
	dialog := u.dialogs[callID]
	_, isPendingAck := u.ackTimers[callID]
	u.mu.Unlock()

	if dialog == nil || !isPendingAck {
		return
	}
	// The 487 answers the INVITE, not the CANCEL, so it is built from the
	// INVITE — same Via, and the INVITE's CSeq.
	u.send(build487(dialog.invite, dialog.LocalTag), addr)

	go u.cleanup(callID)
}

func (u *UAS) handleInfo(msg *sipMessage, addr *net.UDPAddr) {
	u.send(build200OK(msg), addr)

	u.mu.Lock()
	dialog := u.dialogs[msg.callID()]
	u.mu.Unlock()

	if dialog != nil {
		if digit := parseInfoDTMF(msg); digit != "" {
			dialog.RTP.PushDTMF(digit)
		}
	}
}

func (u *UAS) handleBye(msg *sipMessage, addr *net.UDPAddr) {
	u.send(build200OK(msg), addr)

	callID := msg.callID()
	u.mu.Lock()
	if timer, ok := u.ackTimers[callID]; ok {
		timer.Stop()
		delete(u.ackTimers, callID)
	}
	if timer, ok := u.answerTimer[callID]; ok {
		timer.Stop()
		delete(u.answerTimer, callID)
	}
	dialog := u.dialogs[callID]
	u.mu.Unlock()

	if dialog == nil {
		return
	}
	dialog.markRemoteBye()
	go u.endCall(dialog)
}

//
// Call lifecycle.
//

func (u *UAS) startCall(dialog *Dialog) {
	if err := dialog.startMedia(); err != nil {
		u.log.Error("media failed to start", "callId", dialog.CallID, "error", err)
		if u.OnCallFailed != nil {
			u.OnCallFailed(dialog, err)
		}
		u.cleanup(dialog.CallID)
		return
	}

	go u.runMedia(dialog)
	if u.cfg.RTPDeadTimeout > 0 {
		go u.watchForDeadMedia(dialog)
	}
	if u.OnCallStarted != nil {
		u.OnCallStarted(dialog)
	}
}

func (u *UAS) runMedia(dialog *Dialog) {
	dialog.RTP.Run()

	u.mu.Lock()
	_, isActive := u.dialogs[dialog.CallID]
	u.mu.Unlock()
	if isActive {
		u.endCall(dialog)
	}
}

// watchForDeadMedia ends a call whose audio has stopped. A dialog can stay up
// indefinitely after the media path breaks, and the caller hears nothing while
// it does.
func (u *UAS) watchForDeadMedia(dialog *Dialog) {
	interval := min(u.cfg.RTPDeadTimeout/2, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-dialog.Stopped:
			return
		case <-ticker.C:
		}

		u.mu.Lock()
		_, isActive := u.dialogs[dialog.CallID]
		u.mu.Unlock()
		if !isActive {
			return
		}
		if silent := time.Since(dialog.RTP.LastPacketAt()); silent >= u.cfg.RTPDeadTimeout {
			u.log.Warn("media went dead", "callId", dialog.CallID, "silentFor", silent)
			u.endCall(dialog)
			return
		}
	}
}

// endCall runs the ended callback exactly once and releases everything.
func (u *UAS) endCall(dialog *Dialog) {
	u.mu.Lock()
	_, isActive := u.dialogs[dialog.CallID]
	delete(u.dialogs, dialog.CallID)
	u.mu.Unlock()
	if !isActive {
		return
	}

	dialog.Stop()
	if u.OnCallEnded != nil {
		u.OnCallEnded(dialog)
	}
	u.releaseRTPPort(dialog.LocalRTPPort)
}

// cleanup drops a call that never started, so no ended callback fires.
func (u *UAS) cleanup(callID string) {
	u.mu.Lock()
	dialog := u.dialogs[callID]
	delete(u.dialogs, callID)
	for _, timers := range []map[string]*time.Timer{u.ackTimers, u.answerTimer} {
		if timer, ok := timers[callID]; ok {
			timer.Stop()
			delete(timers, callID)
		}
	}
	u.mu.Unlock()

	if dialog != nil {
		dialog.Stop()
		u.releaseRTPPort(dialog.LocalRTPPort)
	}
}

func newTag() string { return fmt.Sprintf("aicc-%d", rand.Uint32()) }

func tagFrom(header string) string {
	for part := range strings.SplitSeq(header, ";") {
		if tag, found := strings.CutPrefix(strings.TrimSpace(part), "tag="); found {
			return tag
		}
	}
	return ""
}
