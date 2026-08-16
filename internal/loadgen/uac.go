// SPDX-License-Identifier: Apache-2.0

// Package loadgen places SIP calls at the application's voice leg and measures
// what comes back.
//
// It is the user agent the switch would be: an INVITE carrying the headers
// FreeSWITCH puts on a bot leg, a PCMU offer, twenty-millisecond frames
// uplink, and a stopwatch on every frame downlink. What it asserts is pacing —
// audio the caller would hear as smooth has to arrive smoothly, and at two
// hundred calls that is a property of the whole process, not of one call.
package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// frameInterval is the packetisation of every G.711 leg in this project.
const frameInterval = 20 * time.Millisecond

// CallResult is what one call reported.
type CallResult struct {
	Err error

	SetupMs      float64
	FirstAudioMs float64 // from answer to the first downlink frame; -1 if none

	FramesSent     int
	FramesReceived int

	// LateFrames counts downlink gaps longer than LateAfter. A jitter buffer
	// on the far side would have to fill those, and the caller would hear it.
	LateFrames int
	MaxGapMs   float64
}

// CallConfig is one call's shape.
type CallConfig struct {
	// Target is the application's SIP address, host:port.
	Target string
	// DID the call arrives on, and the language it is answered in. The
	// orchestrator resolves the flow from these exactly as it does for a real
	// leg from the switch.
	DID      string
	Language string
	From     string

	// Duration the call stays up for once answered.
	Duration time.Duration
	// LateAfter is the downlink gap that counts as late. Two frame intervals
	// leaves room for ordinary scheduling and catches real stalls.
	LateAfter time.Duration
}

func (c *CallConfig) applyDefaults() {
	if c.DID == "" {
		c.DID = "95001"
	}
	if c.Language == "" {
		c.Language = "en"
	}
	if c.From == "" {
		c.From = "10000000000"
	}
	if c.Duration <= 0 {
		c.Duration = 30 * time.Second
	}
	if c.LateAfter <= 0 {
		c.LateAfter = 2 * frameInterval
	}
}

// PlaceCall runs one call start to finish and reports what happened.
func PlaceCall(ctx context.Context, cfg CallConfig) CallResult {
	cfg.applyDefaults()
	result := CallResult{FirstAudioMs: -1}

	target, err := net.ResolveUDPAddr("udp", cfg.Target)
	if err != nil {
		result.Err = fmt.Errorf("resolve %s: %w", cfg.Target, err)
		return result
	}
	sip, err := net.DialUDP("udp", nil, target)
	if err != nil {
		result.Err = fmt.Errorf("dial sip: %w", err)
		return result
	}
	defer sip.Close()

	rtp, err := net.ListenUDP("udp", &net.UDPAddr{IP: sip.LocalAddr().(*net.UDPAddr).IP})
	if err != nil {
		result.Err = fmt.Errorf("open rtp: %w", err)
		return result
	}
	defer rtp.Close()

	call := &uac{cfg: cfg, sip: sip, rtp: rtp}
	call.identify()

	startedAt := time.Now()
	answer, err := call.invite(ctx)
	if err != nil {
		result.Err = err
		return result
	}
	result.SetupMs = float64(time.Since(startedAt).Microseconds()) / 1000

	remoteRTP, err := mediaAddress(answer)
	if err != nil {
		result.Err = err
		_ = call.bye()
		return result
	}

	call.stream(ctx, remoteRTP, &result)
	_ = call.bye()
	return result
}

// uac is one dialog and its media socket.
type uac struct {
	cfg CallConfig
	sip *net.UDPConn
	rtp *net.UDPConn

	callID   string
	tag      string
	branch   string
	aiccCall string
	cseq     int

	toTag   string
	contact string
}

func (u *uac) identify() {
	u.callID = uuid.NewString()
	u.tag = uuid.NewString()[:8]
	u.branch = "z9hG4bK" + uuid.NewString()[:12]
	u.aiccCall = uuid.NewString()
	u.cseq = 1
}

func (u *uac) local() string { return u.sip.LocalAddr().String() }

// invite offers PCMU and waits for the answer, acknowledging it.
func (u *uac) invite(ctx context.Context) (string, error) {
	offer := strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=- %d %d IN IP4 %s", time.Now().Unix(), 1, u.rtpIP()),
		"s=loadgen",
		fmt.Sprintf("c=IN IP4 %s", u.rtpIP()),
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP 0 101", u.rtpPort()),
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-16",
		"a=sendrecv",
		"",
	}, "\r\n")

	// The headers a bot leg carries out of the dialplan. Without them the
	// orchestrator has no number to resolve a flow from.
	request := u.message("INVITE", map[string]string{
		"Content-Type":      "application/sdp",
		"X-AICC-Call-Id":    u.aiccCall,
		"X-AICC-Channel-Id": uuid.NewString(),
		"X-AICC-Did":        u.cfg.DID,
		"X-AICC-Language":   u.cfg.Language,
		"X-AICC-Ani":        u.cfg.From,
	}, offer)

	if _, err := u.sip.Write([]byte(request)); err != nil {
		return "", fmt.Errorf("send invite: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	for {
		response, err := u.readSIP(deadline)
		if err != nil {
			return "", fmt.Errorf("await answer: %w", err)
		}
		status := statusOf(response)
		switch {
		case status >= 100 && status < 200:
			continue // ringing, session progress
		case status == 200:
			u.toTag = headerParam(response, "To", "tag")
			u.contact = contactURI(response)
			u.ack()
			return response, nil
		default:
			return "", fmt.Errorf("call rejected with %d", status)
		}
	}
}

// stream sends a tone uplink and times what arrives downlink.
func (u *uac) stream(ctx context.Context, remote *net.UDPAddr, result *CallResult) {
	stop := time.Now().Add(u.cfg.Duration)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(stop) {
		stop = deadline
	}

	received := make(chan time.Time, 1024)
	go func() {
		buffer := make([]byte, 2048)
		for {
			_ = u.rtp.SetReadDeadline(time.Now().Add(time.Until(stop) + time.Second))
			n, _, err := u.rtp.ReadFromUDP(buffer)
			if err != nil {
				close(received)
				return
			}
			if n < 12 {
				continue // not RTP
			}
			select {
			case received <- time.Now():
			default: // the reader below is behind; the gap it misses is its own
			}
		}
	}()

	tone := uplinkTone()
	ssrc := rand.Uint32()
	sequence := uint16(rand.Uint32())
	timestamp := rand.Uint32()
	packet := make([]byte, 12+media.FrameSamples)

	answeredAt := time.Now()
	var lastArrival time.Time
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	for now := range ticker.C {
		if now.After(stop) {
			break
		}

		offset := (int(sequence) * media.FrameSamples) % len(tone)
		frame := tone[offset : offset+media.FrameSamples]
		writeRTPHeader(packet, sequence, timestamp, ssrc)
		copy(packet[12:], frame)
		if _, err := u.rtp.WriteToUDP(packet, remote); err == nil {
			result.FramesSent++
		}
		sequence++
		timestamp += media.FrameSamples

		// Drain whatever arrived since the last tick, measuring the gaps.
	drain:
		for {
			select {
			case arrival, ok := <-received:
				if !ok {
					break drain
				}
				result.FramesReceived++
				if result.FirstAudioMs < 0 {
					result.FirstAudioMs = float64(arrival.Sub(answeredAt).Microseconds()) / 1000
				}
				if !lastArrival.IsZero() {
					gap := arrival.Sub(lastArrival)
					if gap > u.cfg.LateAfter {
						result.LateFrames++
					}
					if ms := float64(gap.Microseconds()) / 1000; ms > result.MaxGapMs {
						result.MaxGapMs = ms
					}
				}
				lastArrival = arrival
			default:
				break drain
			}
		}
	}
}

func (u *uac) ack() {
	u.sip.Write([]byte(u.message("ACK", nil, "")))
}

func (u *uac) bye() error {
	u.cseq++
	_, err := u.sip.Write([]byte(u.message("BYE", nil, "")))
	return err
}

// message renders a request in this dialog.
func (u *uac) message(method string, extra map[string]string, body string) string {
	target := u.contact
	if target == "" || method == "INVITE" {
		target = fmt.Sprintf("sip:%s@%s", u.cfg.DID, u.cfg.Target)
	}
	to := fmt.Sprintf("<sip:%s@%s>", u.cfg.DID, u.cfg.Target)
	if u.toTag != "" {
		to += ";tag=" + u.toTag
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", method, target)
	fmt.Fprintf(&b, "Via: SIP/2.0/UDP %s;branch=%s\r\n", u.local(), u.branch)
	fmt.Fprintf(&b, "From: <sip:%s@loadgen>;tag=%s\r\n", u.cfg.From, u.tag)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Call-ID: %s\r\n", u.callID)
	fmt.Fprintf(&b, "CSeq: %d %s\r\n", u.cseq, method)
	fmt.Fprintf(&b, "Contact: <sip:loadgen@%s>\r\n", u.local())
	fmt.Fprintf(&b, "Max-Forwards: 70\r\n")
	for name, value := range extra {
		fmt.Fprintf(&b, "%s: %s\r\n", name, value)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.WriteString(body)
	return b.String()
}

func (u *uac) readSIP(deadline time.Time) (string, error) {
	if err := u.sip.SetReadDeadline(deadline); err != nil {
		return "", err
	}
	buffer := make([]byte, 8192)
	n, err := u.sip.Read(buffer)
	if err != nil {
		return "", err
	}
	return string(buffer[:n]), nil
}

func (u *uac) rtpIP() string { return u.rtp.LocalAddr().(*net.UDPAddr).IP.String() }
func (u *uac) rtpPort() int  { return u.rtp.LocalAddr().(*net.UDPAddr).Port }

// writeRTPHeader fills a minimal PCMU header in place.
func writeRTPHeader(packet []byte, sequence uint16, timestamp, ssrc uint32) {
	packet[0], packet[1] = 0x80, 0x00 // version 2, payload type 0 (PCMU)
	packet[2], packet[3] = byte(sequence>>8), byte(sequence)
	packet[4], packet[5] = byte(timestamp>>24), byte(timestamp>>16)
	packet[6], packet[7] = byte(timestamp>>8), byte(timestamp)
	packet[8], packet[9] = byte(ssrc>>24), byte(ssrc>>16)
	packet[10], packet[11] = byte(ssrc>>8), byte(ssrc)
}

// uplinkTone is one second of 440 Hz in µ-law, shared by every call. Sending
// silence would let a receiver's voice activity detection decide there is
// nothing to hear, which is not the load being tested.
var toneOnce = func() []byte {
	samples := media.RateTelephone
	pcm := make([]int16, samples)
	for i := range pcm {
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(media.RateTelephone)))
	}
	converter, err := media.NewConverter(
		media.PCM16Format(media.RateTelephone), media.G711Format(media.LawMu))
	if err != nil {
		panic(err)
	}
	return converter.Convert(make([]byte, 0, samples), media.PCM16ToBytes(nil, pcm))
}()

func uplinkTone() []byte { return toneOnce }

// --- response parsing, only as much as a load generator needs ---------------

func statusOf(response string) int {
	line, _, _ := strings.Cut(response, "\r\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	code, _ := strconv.Atoi(fields[1])
	return code
}

func headerValue(message, name string) string {
	for line := range strings.SplitSeq(message, "\r\n") {
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func headerParam(message, name, param string) string {
	for part := range strings.SplitSeq(headerValue(message, name), ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && key == param {
			return value
		}
	}
	return ""
}

func contactURI(message string) string {
	contact := headerValue(message, "Contact")
	if start := strings.Index(contact, "<"); start >= 0 {
		if end := strings.Index(contact[start:], ">"); end > 0 {
			return contact[start+1 : start+end]
		}
	}
	return contact
}

// mediaAddress reads where to send audio out of the answer's SDP.
func mediaAddress(answer string) (*net.UDPAddr, error) {
	_, body, found := strings.Cut(answer, "\r\n\r\n")
	if !found {
		return nil, errors.New("answer carried no sdp")
	}
	host, port := "", 0
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			host = strings.TrimPrefix(line, "c=IN IP4 ")
		case strings.HasPrefix(line, "m=audio "):
			fields := strings.Fields(line)
			if len(fields) > 1 {
				port, _ = strconv.Atoi(fields[1])
			}
		}
	}
	if host == "" || port == 0 {
		return nil, fmt.Errorf("answer has no media address (host %q port %d)", host, port)
	}
	return net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
}
