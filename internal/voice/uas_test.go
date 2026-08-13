// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// peer stands in for the switch: it sends signalling from one socket and
// exchanges audio on another, exactly as FreeSWITCH does.
type peer struct {
	t       *testing.T
	sip     *net.UDPConn
	rtp     *net.UDPConn
	uasAddr *net.UDPAddr
	callID  string
	cseq    int
}

type callHooks struct {
	started chan *Dialog
	ended   chan *Dialog
	failed  chan error
}

func startUAS(t *testing.T, adjust func(*Config)) (*UAS, *callHooks, *peer) {
	t.Helper()

	cfg := DefaultConfig()
	cfg.SIPHost = "127.0.0.1"
	cfg.SIPPort = 0 // let the OS pick, so tests never collide
	cfg.AdvertiseIP = "127.0.0.1"
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if adjust != nil {
		adjust(&cfg)
	}

	uas := NewUAS(cfg)
	hooks := &callHooks{
		started: make(chan *Dialog, 4),
		ended:   make(chan *Dialog, 4),
		failed:  make(chan error, 4),
	}
	uas.OnCallStarted = func(d *Dialog) { hooks.started <- d }
	uas.OnCallEnded = func(d *Dialog) { hooks.ended <- d }
	uas.OnCallFailed = func(_ *Dialog, err error) { hooks.failed <- err }

	if err := uas.Start(); err != nil {
		t.Fatalf("start uas: %v", err)
	}
	t.Cleanup(uas.Stop)

	return uas, hooks, newPeer(t, uas.LocalPort())
}

func newPeer(t *testing.T, uasPort int) *peer {
	t.Helper()

	sip, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("peer sip socket: %v", err)
	}
	rtp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("peer rtp socket: %v", err)
	}
	t.Cleanup(func() { sip.Close(); rtp.Close() })

	return &peer{
		t:       t,
		sip:     sip,
		rtp:     rtp,
		uasAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: uasPort},
		callID:  fmt.Sprintf("test-%d", uasPort),
		cseq:    1,
	}
}

func (p *peer) rtpPort() int { return p.rtp.LocalAddr().(*net.UDPAddr).Port }

func (p *peer) offer() string {
	return "v=0\r\no=peer 1 1 IN IP4 127.0.0.1\r\ns=test\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
		fmt.Sprintf("m=audio %d RTP/AVP 0 8 101\r\n", p.rtpPort()) +
		"a=rtpmap:0 PCMU/8000\r\na=rtpmap:8 PCMA/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\na=ptime:20\r\na=sendrecv\r\n"
}

func (p *peer) request(method, body string) {
	p.t.Helper()

	contentType := ""
	if body != "" {
		contentType = "Content-Type: application/sdp\r\n"
	}
	msg := fmt.Sprintf(
		"%s sip:aicc@127.0.0.1 SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-%s-%d\r\n"+
			"From: <sip:1001@127.0.0.1>;tag=peer-tag\r\n"+
			"To: <sip:aicc@127.0.0.1>\r\n"+
			"Call-ID: %s\r\n"+
			"CSeq: %d %s\r\n"+
			"Contact: <sip:1001@127.0.0.1:%d>\r\n"+
			"X-Aicc-Call-Id: 0198-test\r\n"+
			"%s"+
			"Content-Length: %d\r\n\r\n%s",
		method, p.sip.LocalAddr().(*net.UDPAddr).Port, method, p.cseq,
		p.callID, p.cseq, method, p.sip.LocalAddr().(*net.UDPAddr).Port,
		contentType, len(body), body)

	if _, err := p.sip.WriteToUDP([]byte(msg), p.uasAddr); err != nil {
		p.t.Fatalf("send %s: %v", method, err)
	}
}

// await reads one SIP message, failing the test if none arrives.
func (p *peer) await(timeout time.Duration) *sipMessage {
	p.t.Helper()
	msg, err := p.read(timeout)
	if err != nil {
		p.t.Fatalf("waiting for a SIP message: %v", err)
	}
	return msg
}

func (p *peer) read(timeout time.Duration) (*sipMessage, error) {
	buf := make([]byte, 65536)
	_ = p.sip.SetReadDeadline(time.Now().Add(timeout))
	n, _, err := p.sip.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return parseSIP(buf[:n])
}

// awaitStatus skips provisional responses and returns the first one matching.
func (p *peer) awaitStatus(code int, timeout time.Duration) *sipMessage {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg, err := p.read(time.Until(deadline))
		if err != nil {
			break
		}
		if msg.statusCode == code {
			return msg
		}
	}
	p.t.Fatalf("no %d response arrived", code)
	return nil
}

// connect drives a full INVITE / 200 / ACK exchange and returns the answer.
func (p *peer) connect(hooks *callHooks) (*sipMessage, *Dialog) {
	p.t.Helper()

	p.request("INVITE", p.offer())
	answer := p.awaitStatus(200, 2*time.Second)
	p.request("ACK", "")

	select {
	case dialog := <-hooks.started:
		return answer, dialog
	case err := <-hooks.failed:
		p.t.Fatalf("call failed to start: %v", err)
	case <-time.After(2 * time.Second):
		p.t.Fatal("the call never started")
	}
	return nil, nil
}

func TestCallSetupAnswersTheOffer(t *testing.T) {
	_, hooks, p := startUAS(t, nil)

	p.request("INVITE", p.offer())

	trying := p.await(time.Second)
	if trying.statusCode != 100 {
		t.Fatalf("first response was %d, want 100 Trying", trying.statusCode)
	}
	answer := p.awaitStatus(200, 2*time.Second)

	offer := parseSDP(answer.body)
	if offer.IP != "127.0.0.1" || offer.Port == 0 {
		t.Errorf("answer advertises %s:%d", offer.IP, offer.Port)
	}
	// The peer offered both laws; the default preference takes µ-law.
	if got := offer.Codecs[0]; got != "PCMU" {
		t.Errorf("answer chose %v, want PCMU", offer.Codecs)
	}
	if offer.DTMFPayloadType != 101 {
		t.Errorf("answer's DTMF payload type = %d, want the offered 101", offer.DTMFPayloadType)
	}
	if !strings.Contains(answer.body, "a=ptime:20") {
		t.Errorf("answer does not state packetisation:\n%s", answer.body)
	}
	// RTP takes an even port so RTCP can have the odd one above it.
	if offer.Port%2 != 0 {
		t.Errorf("RTP port %d is odd, leaving no room for RTCP", offer.Port)
	}

	p.request("ACK", "")
	select {
	case dialog := <-hooks.started:
		if dialog.CustomHeaders["X-Aicc-Call-Id"] != "0198-test" {
			t.Errorf("correlation header lost: %v", dialog.CustomHeaders)
		}
		if dialog.RTP.Law() != media.LawMu {
			t.Errorf("dialog law = %v, want µ-law", dialog.RTP.Law())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the ACK did not start the call")
	}
}

func TestAudioFlowsBothWays(t *testing.T) {
	_, hooks, p := startUAS(t, nil)
	answer, dialog := p.connect(hooks)

	answerSDP := parseSDP(answer.body)
	uasRTP := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: answerSDP.Port}

	// The session paces its own output, so audio arrives even while idle.
	buf := make([]byte, 2048)
	_ = p.rtp.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := p.rtp.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no RTP arrived from the call: %v", err)
	}
	packet, err := unmarshalForTest(buf[:n])
	if err != nil {
		t.Fatalf("the call sent something that is not RTP: %v", err)
	}
	if packet.PayloadType != 0 || len(packet.Payload) != media.FrameSamples {
		t.Errorf("got payload type %d of %d bytes, want µ-law frames of %d",
			packet.PayloadType, len(packet.Payload), media.FrameSamples)
	}
	// Idle audio must be µ-law silence, not zero bytes, which are a loud buzz.
	if packet.Payload[0] != media.LawMu.Silence() {
		t.Errorf("idle frame starts with %#x, want the µ-law silence byte %#x",
			packet.Payload[0], media.LawMu.Silence())
	}

	// Caller audio reaches the consumer.
	tone := make([]byte, media.FrameSamples)
	for i := range tone {
		tone[i] = 0x2A
	}
	inbound := packRTPHeader(make([]byte, 0, 200), 700, 8000, 0xABCD, 0)
	inbound = append(inbound, tone...)
	if _, err := p.rtp.WriteToUDP(inbound, uasRTP); err != nil {
		t.Fatalf("send rtp: %v", err)
	}

	select {
	case frame := <-dialog.RTP.Frames():
		if len(frame) != media.FrameSamples || frame[0] != 0x2A {
			t.Errorf("inbound frame came through as %d bytes starting %#x", len(frame), frame[0])
		}
		media.PutBytes(frame)
	case <-time.After(2 * time.Second):
		t.Fatal("caller audio never reached the consumer")
	}

	// Bot audio reaches the caller: enough frames to clear the prebuffer.
	speech := make([]byte, media.FrameSamples)
	for i := range speech {
		speech[i] = 0x55
	}
	for range prebufferFrames + 2 {
		if !dialog.RTP.Send(speech) {
			t.Fatal("the send queue rejected a frame while empty")
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("queued audio never reached the caller")
		}
		_ = p.rtp.SetReadDeadline(deadline)
		n, _, err := p.rtp.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read rtp: %v", err)
		}
		packet, err := unmarshalForTest(buf[:n])
		if err == nil && len(packet.Payload) > 0 && packet.Payload[0] == 0x55 {
			return
		}
	}
}

// A retransmitted INVITE means our answer was lost. Re-answering identically is
// what the caller is waiting for; dropping it as a duplicate leaves the call
// ringing until it times out.
func TestRetransmittedInviteIsAnsweredIdentically(t *testing.T) {
	uas, _, p := startUAS(t, nil)

	p.request("INVITE", p.offer())
	first := p.awaitStatus(200, 2*time.Second)

	p.request("INVITE", p.offer())
	second := p.awaitStatus(200, 2*time.Second)

	if first.body != second.body {
		t.Errorf("the second answer differs, so a second media session was set up:\n%s\n---\n%s",
			first.body, second.body)
	}
	if got := uas.ActiveCalls(); got != 1 {
		t.Errorf("active calls = %d, want the retransmission folded into one", got)
	}
}

// Nothing below the UAS retransmits a 200 OK to an INVITE, so a single lost
// packet would otherwise leave the caller in silence on a call we think is up.
func TestAnswerIsRetransmittedUntilAcked(t *testing.T) {
	_, _, p := startUAS(t, func(c *Config) {
		c.AckTimeout = 5 * time.Second // long enough to observe a retransmission
	})

	p.request("INVITE", p.offer())
	p.awaitStatus(200, 2*time.Second)

	// The first retransmission is due after 500ms.
	again := p.awaitStatus(200, 2*time.Second)
	if !strings.Contains(again.body, "m=audio") {
		t.Errorf("the retransmission carried no SDP:\n%s", again.body)
	}
}

func TestUnackedCallIsAbandoned(t *testing.T) {
	uas, hooks, p := startUAS(t, func(c *Config) {
		c.AckTimeout = 200 * time.Millisecond
	})

	p.request("INVITE", p.offer())
	p.awaitStatus(200, 2*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for uas.ActiveCalls() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := uas.ActiveCalls(); got != 0 {
		t.Errorf("active calls = %d, want the unacked call released", got)
	}
	// It never started, so it must not be reported as having ended.
	select {
	case <-hooks.ended:
		t.Error("a call that never started was reported as ended")
	default:
	}
}

func TestCancelTerminatesTheInvite(t *testing.T) {
	uas, _, p := startUAS(t, func(c *Config) {
		c.AckTimeout = 5 * time.Second
	})

	p.request("INVITE", p.offer())
	p.awaitStatus(200, 2*time.Second)

	p.cseq++
	p.request("CANCEL", "")

	// Both responses are due: 200 for the CANCEL, 487 for the INVITE.
	var saw200, saw487 bool
	deadline := time.Now().Add(2 * time.Second)
	for (!saw200 || !saw487) && time.Now().Before(deadline) {
		msg, err := p.read(time.Until(deadline))
		if err != nil {
			break
		}
		switch msg.statusCode {
		case 200:
			if strings.Contains(msg.cseq(), "CANCEL") {
				saw200 = true
			}
		case 487:
			saw487 = true
			// The 487 answers the INVITE, so it carries the INVITE's CSeq.
			if !strings.Contains(msg.cseq(), "INVITE") {
				t.Errorf("487 carries CSeq %q, want the INVITE's", msg.cseq())
			}
		}
	}
	if !saw200 {
		t.Error("the CANCEL was never answered")
	}
	if !saw487 {
		t.Error("the INVITE was never terminated with a 487")
	}

	deadline = time.Now().Add(2 * time.Second)
	for uas.ActiveCalls() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := uas.ActiveCalls(); got != 0 {
		t.Errorf("active calls = %d after CANCEL", got)
	}
}

// Answering a BYE with a BYE is a protocol error, and some proxies react to it
// by tearing down something else.
func TestRemoteByeEndsTheCallWithoutSendingOurOwn(t *testing.T) {
	uas, hooks, p := startUAS(t, nil)
	p.connect(hooks)

	p.cseq++
	p.request("BYE", "")

	if got := p.awaitStatus(200, 2*time.Second); got == nil {
		t.Fatal("the BYE was not acknowledged")
	}
	select {
	case <-hooks.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("the call was never reported as ended")
	}

	// Nothing further should arrive on the signalling socket.
	if msg, err := p.read(500 * time.Millisecond); err == nil && msg.method == methodBye {
		t.Error("a BYE was sent in response to the peer's BYE")
	}
	if got := uas.ActiveCalls(); got != 0 {
		t.Errorf("active calls = %d after the call ended", got)
	}
}

func TestHangingUpSendsBye(t *testing.T) {
	_, hooks, p := startUAS(t, nil)
	_, dialog := p.connect(hooks)

	dialog.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := p.read(time.Until(deadline))
		if err != nil {
			break
		}
		if msg.method == methodBye {
			if msg.callID() != p.callID {
				t.Errorf("BYE carries Call-ID %q, want %q", msg.callID(), p.callID)
			}
			return
		}
	}
	t.Error("no BYE was sent when the call was ended locally")
}

func TestCapacityLimitIsEnforced(t *testing.T) {
	_, hooks, p := startUAS(t, func(c *Config) { c.MaxCalls = 1 })
	p.connect(hooks)

	second := newPeer(t, p.uasAddr.Port)
	second.callID = "test-second"
	second.request("INVITE", second.offer())

	response := second.awaitStatus(486, 2*time.Second)
	if response.statusCode != 486 {
		t.Errorf("second call got %d, want 486 Busy Here", response.statusCode)
	}
}

func TestOfferWithNoCommonCodecIsRejected(t *testing.T) {
	_, _, p := startUAS(t, nil)

	p.request("INVITE", "v=0\r\nc=IN IP4 127.0.0.1\r\nm=audio 5004 RTP/AVP 9\r\n"+
		"a=rtpmap:9 G722/8000\r\n")

	if got := p.awaitStatus(488, 2*time.Second); got.statusCode != 488 {
		t.Errorf("got %d, want 488 Not Acceptable Here", got.statusCode)
	}
}

func TestInviteWithoutMediaAddressIsRejected(t *testing.T) {
	_, _, p := startUAS(t, nil)

	p.request("INVITE", "v=0\r\ns=broken\r\n")

	if got := p.awaitStatus(488, 2*time.Second); got.statusCode != 488 {
		t.Errorf("got %d, want 488", got.statusCode)
	}
}

func TestOptionsIsAnswered(t *testing.T) {
	_, _, p := startUAS(t, nil)

	p.request("OPTIONS", "")

	response := p.awaitStatus(200, 2*time.Second)
	if !strings.Contains(response.get("Allow"), "INVITE") {
		t.Errorf("OPTIONS response does not advertise methods: %q", response.get("Allow"))
	}
}

func TestInfoDigitsReachTheSameStreamAsRFC2833(t *testing.T) {
	_, hooks, p := startUAS(t, nil)
	_, dialog := p.connect(hooks)

	p.cseq++
	body := "Signal=4\r\nDuration=250\r\n"
	msg := fmt.Sprintf(
		"INFO sip:aicc@127.0.0.1 SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-info\r\n"+
			"From: <sip:1001@127.0.0.1>;tag=peer-tag\r\n"+
			"To: <sip:aicc@127.0.0.1>\r\nCall-ID: %s\r\nCSeq: %d INFO\r\n"+
			"Content-Type: application/dtmf-relay\r\n"+
			"Content-Length: %d\r\n\r\n%s",
		p.sip.LocalAddr().(*net.UDPAddr).Port, p.callID, p.cseq, len(body), body)
	if _, err := p.sip.WriteToUDP([]byte(msg), p.uasAddr); err != nil {
		t.Fatalf("send INFO: %v", err)
	}

	select {
	case digit := <-dialog.RTP.DTMF():
		if digit != "4" {
			t.Errorf("got digit %q, want 4", digit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a digit sent by INFO never surfaced")
	}
}

// A dialog can stay up long after the media path breaks, and the caller hears
// nothing at all while it does.
func TestDeadMediaEndsTheCall(t *testing.T) {
	_, hooks, p := startUAS(t, func(c *Config) {
		c.RTPDeadTimeout = 300 * time.Millisecond
	})
	p.connect(hooks)

	select {
	case <-hooks.ended:
	case <-time.After(3 * time.Second):
		t.Fatal("a call with no inbound media was never ended")
	}
}

func TestRTPPortsAreAllocatedInPairsAndReleased(t *testing.T) {
	uas := NewUAS(Config{
		RTPPortRange: [2]int{41000, 41003}, // exactly two pairs
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	first, err := uas.allocateRTPPort()
	if err != nil {
		t.Fatalf("first allocation: %v", err)
	}
	second, err := uas.allocateRTPPort()
	if err != nil {
		t.Fatalf("second allocation: %v", err)
	}
	if first%2 != 0 || second%2 != 0 {
		t.Errorf("allocated odd ports %d and %d, leaving no room for RTCP", first, second)
	}
	if first == second || first+1 == second || second+1 == first {
		t.Errorf("ports %d and %d overlap, so one call's RTCP lands on another's RTP",
			first, second)
	}

	if _, err := uas.allocateRTPPort(); err == nil {
		t.Error("allocated a third pair from a two-pair range")
	}

	uas.releaseRTPPort(first)
	if reused, err := uas.allocateRTPPort(); err != nil || reused != first {
		t.Errorf("after release got port %d (%v), want %d back", reused, err, first)
	}
}
