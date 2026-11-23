// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// uacResponse is a response the test caller received.
type uacResponse struct {
	status int
	method string // the CSeq method: INVITE, CANCEL
	at     time.Time
	msg    *voice.SIPMessage
}

type rtpArrival struct {
	at  time.Time
	rms float64
}

// testUAC is the switch's side of one call: it sends the INVITE, acknowledges
// finals, answers a BYE, and exchanges audio on its own RTP socket.
type testUAC struct {
	t      *testing.T
	sip    *net.UDPConn
	rtp    *net.UDPConn
	target *net.UDPAddr
	number string

	callID, tag, branch string
	offerCodecs         string

	noAck bool // withhold the ACK, to see the 200 retransmitted

	mu        sync.Mutex
	responses []uacResponse
	byes      int
	uplink    []rtpArrival
	toTag     string
}

// newUAC binds the caller's sockets on host.
func newUAC(t *testing.T, host string, target *net.UDPAddr, number string) *testUAC {
	t.Helper()
	sip, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host)})
	if err != nil {
		t.Fatal(err)
	}
	rtp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host)})
	if err != nil {
		t.Fatal(err)
	}
	u := &testUAC{
		t: t, sip: sip, rtp: rtp, target: target, number: number,
		callID: uuid.NewString(), tag: uuid.NewString()[:8], branch: "z9hG4bK" + uuid.NewString()[:10],
		offerCodecs: "0 8 101",
	}
	t.Cleanup(func() { sip.Close(); rtp.Close() })
	go u.readSIP()
	go u.readRTP()
	return u
}

func (u *testUAC) local() string { return u.sip.LocalAddr().String() }

func (u *testUAC) offer() string {
	lines := []string{
		"v=0",
		"o=- 1 1 IN IP4 " + u.rtp.LocalAddr().(*net.UDPAddr).IP.String(),
		"s=testuac",
		"c=IN IP4 " + u.rtp.LocalAddr().(*net.UDPAddr).IP.String(),
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP %s", u.rtp.LocalAddr().(*net.UDPAddr).Port, u.offerCodecs),
	}
	for _, pt := range strings.Fields(u.offerCodecs) {
		switch pt {
		case "0":
			lines = append(lines, "a=rtpmap:0 PCMU/8000")
		case "8":
			lines = append(lines, "a=rtpmap:8 PCMA/8000")
		case "101":
			lines = append(lines, "a=rtpmap:101 telephone-event/8000", "a=fmtp:101 0-16")
		}
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

func (u *testUAC) request(method string, cseq int, branch string, extra string, body string) string {
	to := fmt.Sprintf("<sip:%s@%s>", u.number, u.target)
	u.mu.Lock()
	if u.toTag != "" {
		to += ";tag=" + u.toTag
	}
	u.mu.Unlock()
	return fmt.Sprintf("%s sip:%s@%s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s;branch=%s\r\n"+
		"From: <sip:caller@testuac>;tag=%s\r\n"+
		"To: %s\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d %s\r\n"+
		"Contact: <sip:caller@%s>\r\n"+
		"Max-Forwards: 70\r\n"+
		"%s"+
		"Content-Length: %d\r\n\r\n%s",
		method, u.number, u.target, u.local(), branch, u.tag, to, u.callID, cseq, method,
		u.local(), extra, len(body), body)
}

// write sends one request. A write on the closed socket is the reader's ACK
// racing the test's cleanup, not a failure: by then the test has its verdict,
// and reporting it would fail a passing test or log after it completed.
func (u *testUAC) write(s string) {
	if _, err := u.sip.WriteToUDP([]byte(s), u.target); err != nil && !errors.Is(err, net.ErrClosed) {
		u.t.Errorf("uac write: %v", err)
	}
}

// invite sends the INVITE with the headers a dialplan would stamp on it.
func (u *testUAC) invite() {
	u.write(u.request("INVITE", 1, u.branch,
		"Content-Type: application/sdp\r\nX-AICC-Attempt-ID: attempt-"+u.number+"\r\n", u.offer()))
}

func (u *testUAC) cancel() { u.write(u.request("CANCEL", 1, u.branch, "", "")) }

func (u *testUAC) bye() {
	u.write(u.request("BYE", 2, "z9hG4bK"+uuid.NewString()[:10], "", ""))
}

func (u *testUAC) ack() { u.write(u.request("ACK", 1, u.branch, "", "")) }

func (u *testUAC) readSIP() {
	buf := make([]byte, 65536)
	for {
		n, from, err := u.sip.ReadFromUDP(buf)
		if err != nil {
			return
		}
		msg, err := voice.ParseSIP(append([]byte(nil), buf[:n]...))
		if err != nil {
			continue
		}
		if msg.StatusCode() == 0 {
			if msg.Method() == "BYE" {
				u.mu.Lock()
				u.byes++
				u.mu.Unlock()
				_, _ = u.sip.WriteToUDP(msg.Response("200 OK", "", "", "", nil), from)
			}
			continue
		}
		method := strings.Fields(msg.CSeq())[1]
		u.mu.Lock()
		u.responses = append(u.responses, uacResponse{status: msg.StatusCode(), method: method, at: time.Now(), msg: msg})
		if msg.StatusCode() >= 100 {
			if tag := tagOf(msg.To()); tag != "" {
				u.toTag = tag
			}
		}
		withheld := u.noAck
		u.mu.Unlock()
		if method == "INVITE" && msg.StatusCode() >= 300 || method == "INVITE" && msg.StatusCode() == 200 && !withheld {
			u.ack()
		}
	}
}

func tagOf(header string) string {
	_, tag, _ := strings.Cut(header, "tag=")
	tag, _, _ = strings.Cut(tag, ";")
	return tag
}

func (u *testUAC) readRTP() {
	buf := make([]byte, 2048)
	for {
		n, _, err := u.rtp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n <= 12 {
			continue
		}
		u.mu.Lock()
		u.uplink = append(u.uplink, rtpArrival{at: time.Now(), rms: frameRMS(media.LawMu, buf[12:n])})
		u.mu.Unlock()
	}
}

// statuses lists the INVITE responses received so far, in order.
func (u *testUAC) statuses() []int {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []int
	for _, r := range u.responses {
		if r.method == "INVITE" {
			out = append(out, r.status)
		}
	}
	return out
}

func (u *testUAC) all() []uacResponse {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]uacResponse(nil), u.responses...)
}

// waitResponse waits for a response with this status and CSeq method.
func (u *testUAC) waitResponse(status int, method string) uacResponse {
	u.t.Helper()
	var found uacResponse
	eventually(u.t, fmt.Sprintf("a %d to the %s", status, method), func() bool {
		for _, r := range u.all() {
			if r.status == status && r.method == method {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// waitFinal waits for the INVITE's final response.
func (u *testUAC) waitFinal() uacResponse {
	u.t.Helper()
	var found uacResponse
	eventually(u.t, "a final response", func() bool {
		for _, r := range u.all() {
			if r.method == "INVITE" && r.status >= 200 {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

func (u *testUAC) byeCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.byes
}

func (u *testUAC) uplinkFrames() []rtpArrival {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]rtpArrival(nil), u.uplink...)
}

// remoteRTP is where the simulator said to send audio.
func remoteRTP(t *testing.T, r uacResponse) *net.UDPAddr {
	t.Helper()
	offer := voice.ParseSDP(r.msg.Body())
	if offer.Port == 0 {
		t.Fatalf("response %d carried no media address: %q", r.status, r.msg.Body())
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(offer.IP, fmt.Sprint(offer.Port)))
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// sendDownlink plays the caller's audio: silent frames for lead, then a tone
// for toneFor. It returns the number of frames sent when the last frame is
// sent or ctx ends — under load the pacing lags, so a receiver-side check
// measures against what was sent, not what the durations asked for.
func (u *testUAC) sendDownlink(ctx context.Context, remote *net.UDPAddr, lead, toneFor time.Duration) int {
	tone := newToneSource(media.LawMu, 440)
	silence := media.SilenceFrame(media.LawMu)
	packet := make([]byte, 12+media.FrameSamples)
	packet[0] = 0x80
	var seq uint16 = 1000
	var ts uint32 = 16000
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for now := range ticker.C {
		elapsed := now.Sub(start)
		if elapsed >= lead+toneFor || ctx.Err() != nil {
			return int(seq) - 1000
		}
		frame := silence
		if elapsed >= lead {
			frame = tone.next()
		}
		packet[2], packet[3] = byte(seq>>8), byte(seq)
		packet[4], packet[5], packet[6], packet[7] = byte(ts>>24), byte(ts>>16), byte(ts>>8), byte(ts)
		packet[11] = 7
		copy(packet[12:], frame)
		_, _ = u.rtp.WriteToUDP(packet, remote)
		seq++
		ts += media.FrameSamples
	}
	return int(seq) - 1000
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
