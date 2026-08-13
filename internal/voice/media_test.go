// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

const switchOffer = "v=0\r\n" +
	"o=FreeSWITCH 1 1 IN IP4 10.0.0.8\r\n" +
	"s=FreeSWITCH\r\n" +
	"c=IN IP4 10.0.0.8\r\n" +
	"t=0 0\r\n" +
	"m=audio 24586 RTP/AVP 0 8 96\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n" +
	"a=rtpmap:96 telephone-event/8000\r\n" +
	"a=fmtp:96 0-16\r\n" +
	"a=ptime:20\r\n" +
	"a=sendrecv\r\n"

func TestParseSDP(t *testing.T) {
	offer := parseSDP(switchOffer)

	if offer.IP != "10.0.0.8" || offer.Port != 24586 {
		t.Errorf("media address = %s:%d", offer.IP, offer.Port)
	}
	// Payload type 0 came with no rtpmap, as a peer is entitled to send it.
	if got := offer.Codecs[0]; got != "PCMU" {
		t.Errorf("static payload type 0 resolved to %q, want PCMU", got)
	}
	if got := offer.Codecs[8]; got != "PCMA" {
		t.Errorf("payload type 8 resolved to %q, want PCMA", got)
	}
	// The DTMF type is 96 here; assuming 101 is how digits silently vanish.
	if offer.DTMFPayloadType != 96 {
		t.Errorf("DTMF payload type = %d, want 96", offer.DTMFPayloadType)
	}
	if _, isCodec := offer.Codecs[96]; isCodec {
		t.Error("telephone-event was treated as an audio codec")
	}
}

func TestParseSDPIgnoresOtherMediaSections(t *testing.T) {
	sdp := switchOffer + "m=video 30000 RTP/AVP 99\r\na=rtpmap:99 H264/90000\r\n"
	offer := parseSDP(sdp)

	if _, present := offer.Codecs[99]; present {
		t.Error("a video codec was picked up as audio")
	}
	if offer.Port != 24586 {
		t.Errorf("the audio port was overwritten by the video section: %d", offer.Port)
	}
}

func TestParseSDPWithoutDTMF(t *testing.T) {
	sdp := "c=IN IP4 10.0.0.8\r\nm=audio 5000 RTP/AVP 0\r\n"
	if got := parseSDP(sdp).DTMFPayloadType; got != -1 {
		t.Errorf("DTMF payload type = %d, want -1 when none is offered", got)
	}
}

func TestNegotiationFollowsLocalPreference(t *testing.T) {
	offer := parseSDP(switchOffer) // offers both laws

	law, pt, ok := negotiate(offer, []media.Law{media.LawAlaw, media.LawMu})
	if !ok || law != media.LawAlaw || pt != 8 {
		t.Errorf("got law %v pt %d ok %v, want A-law on 8", law, pt, ok)
	}

	law, pt, ok = negotiate(offer, []media.Law{media.LawMu, media.LawAlaw})
	if !ok || law != media.LawMu || pt != 0 {
		t.Errorf("got law %v pt %d ok %v, want µ-law on 0", law, pt, ok)
	}
}

func TestNegotiationFailsWithNoCommonCodec(t *testing.T) {
	offer := parseSDP("c=IN IP4 10.0.0.8\r\nm=audio 5000 RTP/AVP 9\r\na=rtpmap:9 G722/8000\r\n")
	if _, _, ok := negotiate(offer, []media.Law{media.LawMu, media.LawAlaw}); ok {
		t.Error("negotiated a codec that was never offered")
	}
}

func TestSDPAnswer(t *testing.T) {
	answer := buildSDPAnswer("10.0.0.5", 40002, 7, media.LawAlaw, 8, 96)

	for _, want := range []string{
		"c=IN IP4 10.0.0.5",
		"m=audio 40002 RTP/AVP 8 96",
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:96 telephone-event/8000",
		"a=fmtp:96 0-16",
		// Stated explicitly so the peer does not fall back to its own default
		// packetisation, which the send loop is not paced for.
		"a=ptime:20",
		"a=sendrecv",
	} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer is missing %q:\n%s", want, answer)
		}
	}
	if !strings.HasSuffix(answer, "\r\n") {
		t.Error("answer does not end with CRLF")
	}
}

func TestSDPAnswerOmitsDTMFWhenNotNegotiated(t *testing.T) {
	answer := buildSDPAnswer("10.0.0.5", 40002, 7, media.LawMu, 0, -1)

	if strings.Contains(answer, "telephone-event") {
		t.Errorf("advertised DTMF that was never offered:\n%s", answer)
	}
	if !strings.Contains(answer, "m=audio 40002 RTP/AVP 0\r\n") {
		t.Errorf("format list still lists a DTMF type:\n%s", answer)
	}
}

//
// Jitter buffer.
//

// frameOf builds a pooled payload the buffer can take ownership of.
func frameOf(marker byte) []byte {
	frame := media.GetBytes(media.FrameSamples)
	for range media.FrameSamples {
		frame = append(frame, marker)
	}
	return frame
}

func markerOf(frame []byte) byte { return frame[0] }

func markersOf(frames [][]byte) []byte {
	out := make([]byte, 0, len(frames))
	for _, f := range frames {
		out = append(out, markerOf(f))
	}
	return out
}

func TestJitterPassesOrderedAudioStraightThrough(t *testing.T) {
	j := newJitterBuffer(media.LawMu)

	for i := range 5 {
		out := j.push(uint16(100+i), frameOf(byte(i)))
		if len(out) != 1 || markerOf(out[0]) != byte(i) {
			t.Fatalf("frame %d: got %v, want it passed through immediately", i, markersOf(out))
		}
	}
	if lost, dropped, filled := j.stats(); lost|dropped|filled != 0 {
		t.Errorf("an in-order stream produced lost=%d dropped=%d filled=%d", lost, dropped, filled)
	}
}

func TestJitterReordersWithinTheWindow(t *testing.T) {
	j := newJitterBuffer(media.LawMu)
	j.push(100, frameOf(0)) // establishes the sequence

	// 102 arrives before 101, which is ordinary on a LAN.
	if out := j.push(102, frameOf(2)); len(out) != 0 {
		t.Fatalf("the out-of-order frame was played early: %v", markersOf(out))
	}
	out := j.push(101, frameOf(1))
	if got := markersOf(out); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, want both frames in order once the gap filled", got)
	}
	if _, _, filled := j.stats(); filled != 0 {
		t.Error("silence was inserted for a frame that did arrive")
	}
}

func TestJitterDropsFramesThatArriveTooLate(t *testing.T) {
	j := newJitterBuffer(media.LawMu)
	j.push(100, frameOf(0))
	j.push(101, frameOf(1))

	// Its slot has already been played; playing it now would be worse.
	if out := j.push(100, frameOf(9)); len(out) != 0 {
		t.Fatalf("a late frame was played out of order: %v", markersOf(out))
	}
	if _, dropped, _ := j.stats(); dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestJitterFillsSmallGapsWithCodecCorrectSilence(t *testing.T) {
	j := newJitterBuffer(media.LawAlaw)
	j.push(100, frameOf(0))

	// Frames 101 and 102 never arrive. Once the window fills, waiting stops.
	var out [][]byte
	for i := range reorderWindowFrames {
		out = append(out, j.push(uint16(103+i), frameOf(byte(3+i)))...)
	}
	if len(out) < 2 {
		t.Fatalf("the buffer never gave up waiting: %v", markersOf(out))
	}
	// The fill must be A-law silence, not zero bytes, which are audible.
	for i := range 2 {
		if got := markerOf(out[i]); got != media.LawAlaw.Silence() {
			t.Errorf("gap frame %d is %#x, want the A-law silence byte %#x",
				i, got, media.LawAlaw.Silence())
		}
	}
	if lost, _, filled := j.stats(); lost != 2 || filled != 2 {
		t.Errorf("lost = %d filled = %d, want 2 and 2", lost, filled)
	}
}

func TestJitterResyncsRatherThanFloodingSilence(t *testing.T) {
	j := newJitterBuffer(media.LawMu)
	j.push(100, frameOf(0))

	// A gap this large is the stream restarting, not packet loss. Filling it
	// would play half a second of silence before catching up.
	out := j.push(100+maxSilenceGapFrames+10, frameOf(1))
	if got := markersOf(out); len(got) != 1 || got[0] != 1 {
		t.Fatalf("got %d frames %v, want a single resynchronised frame", len(got), got)
	}
	if _, _, filled := j.stats(); filled != 0 {
		t.Errorf("filled %d frames of silence across a stream restart", filled)
	}
}

// Sequence numbers wrap after 65535. Comparing them as plain integers makes
// the buffer treat the wrap as a huge backward jump and drop real audio.
func TestJitterSurvivesSequenceWraparound(t *testing.T) {
	j := newJitterBuffer(media.LawMu)
	j.push(65534, frameOf(0))

	for i, seq := range []uint16{65535, 0, 1} {
		out := j.push(seq, frameOf(byte(i+1)))
		if len(out) != 1 {
			t.Fatalf("seq %d produced %v, want one frame", seq, markersOf(out))
		}
	}
	if _, dropped, _ := j.stats(); dropped != 0 {
		t.Errorf("dropped %d frames across the wrap", dropped)
	}
}

//
// RTP wire format and DTMF.
//

func TestRTPHeaderRoundTrip(t *testing.T) {
	packet := packRTPHeader(make([]byte, 0, 200), 4242, 96000, 0xDEADBEEF, 8)
	packet = append(packet, 0xD5, 0xD5)

	parsed, err := unmarshalForTest(packet)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Version != 2 {
		t.Errorf("version = %d, want 2", parsed.Version)
	}
	if parsed.SequenceNumber != 4242 || parsed.Timestamp != 96000 ||
		parsed.SSRC != 0xDEADBEEF || parsed.PayloadType != 8 {
		t.Errorf("header round-tripped as seq=%d ts=%d ssrc=%#x pt=%d",
			parsed.SequenceNumber, parsed.Timestamp, parsed.SSRC, parsed.PayloadType)
	}
	if len(parsed.Payload) != 2 {
		t.Errorf("payload length = %d, want 2", len(parsed.Payload))
	}
}

func TestParseDTMFEvent(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		digit    string
		isEnd    bool
		duration int
		ok       bool
	}{
		{"digit five in progress", []byte{5, 0x0A, 0x01, 0x40}, "5", false, 320, true},
		{"digit five ended", []byte{5, 0x8A, 0x03, 0x20}, "5", true, 800, true},
		{"star", []byte{10, 0x8A, 0, 0}, "*", true, 0, true},
		{"hash", []byte{11, 0x8A, 0, 0}, "#", true, 0, true},
		{"truncated", []byte{5, 0x8A}, "", false, 0, false},
		{"event out of range", []byte{99, 0x8A, 0, 0}, "", false, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digit, isEnd, duration, ok := parseDTMFEvent(tt.payload)
			if ok != tt.ok || digit != tt.digit || isEnd != tt.isEnd || duration != tt.duration {
				t.Errorf("got (%q, %v, %d, %v), want (%q, %v, %d, %v)",
					digit, isEnd, duration, ok, tt.digit, tt.isEnd, tt.duration, tt.ok)
			}
		})
	}
}

// One keypress produces many packets and three copies of the end packet. The
// timestamp identifies the event, so one digit must reach the consumer.
func TestOneKeypressYieldsOneDigit(t *testing.T) {
	session := NewRTPSession(0, media.LawMu, 101, nil)

	press := func(timestamp uint32) {
		for range 4 { // in-progress packets carry no end bit
			session.handlePacket(dtmfPacket(t, timestamp, []byte{7, 0x0A, 0, 100}))
		}
		for range 3 { // the end packet is sent three times
			session.handlePacket(dtmfPacket(t, timestamp, []byte{7, 0x8A, 0, 200}))
		}
	}
	press(1000)
	press(2000) // a second press of the same key

	digits := drainDigits(session)
	if len(digits) != 2 || digits[0] != "7" || digits[1] != "7" {
		t.Errorf("got %v, want exactly one digit per keypress", digits)
	}
}

func TestUnnegotiatedPayloadTypeIsIgnored(t *testing.T) {
	session := NewRTPSession(0, media.LawMu, 101, nil) // µ-law is payload type 0

	// A-law audio on a µ-law session is not audio this decoder understands;
	// feeding it through would produce noise.
	packet := packRTPHeader(make([]byte, 0, 200), 1, 160, 99, 8)
	packet = append(packet, make([]byte, media.FrameSamples)...)
	session.handlePacket(packet)

	select {
	case frame := <-session.Frames():
		t.Errorf("a frame of the wrong payload type was delivered: %d bytes", len(frame))
	default:
	}
}

func TestSendCopiesTheCallersBuffer(t *testing.T) {
	session := NewRTPSession(0, media.LawMu, -1, nil)

	frame := []byte{1, 2, 3}
	if !session.Send(frame) {
		t.Fatal("Send rejected a frame on an empty queue")
	}
	frame[0] = 99 // the caller reuses its own buffer

	queued := <-session.tx
	if queued[0] != 1 {
		t.Error("Send kept the caller's buffer instead of copying it")
	}
}

func TestClearTxDropsQueuedAudio(t *testing.T) {
	session := NewRTPSession(0, media.LawMu, -1, nil)
	for range 10 {
		session.Send([]byte{1})
	}

	if cleared := session.ClearTx(); cleared != 10 {
		t.Errorf("cleared %d frames, want 10", cleared)
	}
	if len(session.tx) != 0 {
		t.Errorf("%d frames survived the flush", len(session.tx))
	}
}

// The consumer is a live conversation: when it falls behind, the newest audio
// matters and the oldest must go.
func TestInboundQueueDropsTheOldestWhenFull(t *testing.T) {
	session := NewRTPSession(0, media.LawMu, -1, nil)

	total := cap(session.rx) + 10
	for i := range total {
		session.deliver(frameOf(byte(i)))
	}

	if got := len(session.rx); got != cap(session.rx) {
		t.Fatalf("queue holds %d frames, want it capped at %d", got, cap(session.rx))
	}
	first := <-session.rx
	if markerOf(first) == 0 {
		t.Error("the oldest frame survived; the queue dropped new audio instead of stale audio")
	}
}
