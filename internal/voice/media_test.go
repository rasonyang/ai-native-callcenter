// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	sdp := "c=IN IP4 10.0.0.8\r\nm=audio 5000 RTP/AVP 0\r\n"
	if got := parseSDP(sdp).DTMFPayloadType; got != -1 {
		t.Errorf("DTMF payload type = %d, want -1 when none is offered", got)
	}
}

func TestNegotiationFollowsLocalPreference(t *testing.T) {
	t.Parallel()
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

func TestSDPAnswer(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if got := session.RxDropped(); got != 10 {
		t.Errorf("RxDropped = %d, want the 10 frames pushed out", got)
	}
}

func isMediaStarted(s *RTPSession) bool {
	select {
	case <-s.MediaStarted():
		return true
	default:
		return false
	}
}

func TestMediaHasNotStartedBeforeTheFirstPacket(t *testing.T) {
	t.Parallel()
	session := NewRTPSession(0, media.LawMu, 101, nil)
	if isMediaStarted(session) || session.HasReceived() {
		t.Error("media reported as started on a session that has received nothing")
	}
}

func TestTheFirstAudioPacketStartsMedia(t *testing.T) {
	t.Parallel()
	session := NewRTPSession(0, media.LawMu, -1, nil)
	packet := packRTPHeader(make([]byte, 0, 200), 1, 160, 99, 0)
	packet = append(packet, make([]byte, media.FrameSamples)...)
	session.handlePacket(packet)
	if !isMediaStarted(session) || !session.HasReceived() {
		t.Error("the first packet did not start media")
	}
}

// A keypress packet proves the path carries media as surely as audio does.
func TestADTMFPacketAloneStartsMedia(t *testing.T) {
	t.Parallel()
	session := NewRTPSession(0, media.LawMu, 101, nil)
	session.handlePacket(dtmfPacket(t, 1000, []byte{7, 0x0A, 0, 100}))
	if !isMediaStarted(session) {
		t.Error("a DTMF packet did not start media")
	}
}

func TestMediaStartedClosesOnceHoweverManyPacketsFollow(t *testing.T) {
	t.Parallel()
	session := NewRTPSession(0, media.LawMu, 101, nil)
	// A second close would panic, which is what this is here to catch.
	for i := range 500 {
		packet := packRTPHeader(make([]byte, 0, 200), uint16(i+1), uint32(160*(i+1)), 99, 0)
		packet = append(packet, make([]byte, media.FrameSamples)...)
		session.handlePacket(packet)
		if i%50 == 0 {
			for range 3 {
				session.handlePacket(dtmfPacket(t, uint32(i), []byte{1, 0x8A, 0, 200}))
			}
		}
	}
	if !isMediaStarted(session) {
		t.Error("media did not start")
	}
}

func TestFirstMediaAtIsTheFirstPacketsArrival(t *testing.T) {
	t.Parallel()
	session := NewRTPSession(0, media.LawMu, -1, nil)
	if !session.FirstMediaAt().IsZero() {
		t.Error("FirstMediaAt set before any packet")
	}
	before := time.Now()
	packet := packRTPHeader(make([]byte, 0, 200), 1, 160, 99, 0)
	packet = append(packet, make([]byte, media.FrameSamples)...)
	session.handlePacket(packet)
	first := session.FirstMediaAt()
	time.Sleep(10 * time.Millisecond)
	session.handlePacket(packet)
	if first.Before(before) || !session.FirstMediaAt().Equal(first) {
		t.Errorf("FirstMediaAt = %v then %v, want the first packet's time, unchanged", first, session.FirstMediaAt())
	}
}

// arrival is one RTP packet as the far end saw it.
type arrival struct {
	seq     uint16
	ts      uint32
	payload []byte
}

// startPacer runs a pacer against a loopback sink and returns the packets the
// sink receives, in order, and a channel that closes when Run returns. prefill
// audio frames are queued before Run starts; hook is installed as
// beforeTxRecv, so it exists before the first tick and no test has to arm it
// after queueing.
func startPacer(t *testing.T, prefill int, hook func(r *RTPSession)) (*RTPSession, <-chan arrival, <-chan struct{}) {
	t.Helper()
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	session := NewRTPSession(0, media.LawMu, -1, nil)
	if err := session.Start(sink.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("start session: %v", err)
	}
	t.Cleanup(session.Stop)
	if hook != nil {
		session.beforeTxRecv = func() { hook(session) }
	}
	for range prefill {
		session.Send(bytes.Repeat([]byte{0x42}, media.FrameSamples))
	}

	arrivals := make(chan arrival, 8192)
	go func() {
		defer close(arrivals)
		buf := make([]byte, 512)
		for {
			n, _, err := sink.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt, err := unmarshalForTest(buf[:n])
			if err != nil {
				return
			}
			arrivals <- arrival{pkt.SequenceNumber, pkt.Timestamp, append([]byte(nil), pkt.Payload...)}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		session.Run()
	}()
	return session, arrivals, done
}

// pacerWait bounds every wait on the pacer. A blocked pacer never sends again,
// so the bound only has to be far above a starved scheduler, not near 20 ms.
const pacerWait = 5 * time.Second

func nextArrival(t *testing.T, arrivals <-chan arrival) arrival {
	t.Helper()
	select {
	case a, ok := <-arrivals:
		if !ok {
			t.Fatal("sink closed before the next RTP packet")
		}
		return a
	case <-time.After(pacerWait):
		t.Fatal("no RTP packet: the pacer is blocked")
		return arrival{}
	}
}

func waitSeq(t *testing.T, seqs <-chan uint16) uint16 {
	t.Helper()
	select {
	case seq := <-seqs:
		return seq
	case <-time.After(pacerWait):
		t.Fatal("the pacer never reached the barge-in tick")
		return 0
	}
}

// skipTo consumes packets up to, not including, the one with sequence number
// seq, and returns them. The barge-in tick is identified by sequence number
// rather than by draining the socket, so audio sent before it is not mistaken
// for audio sent after it.
func skipTo(t *testing.T, arrivals <-chan arrival, seq uint16) (skipped []arrival, at arrival) {
	t.Helper()
	for {
		a := nextArrival(t, arrivals)
		if a.seq == seq {
			return skipped, a
		}
		skipped = append(skipped, a)
	}
}

// wantSilenceRun checks that first and the n packets after it are silence, one
// sequence number and one frame of RTP time apart: the stream a barge-in must
// leave, whatever the queue did.
func wantSilenceRun(t *testing.T, arrivals <-chan arrival, first arrival, n int) {
	t.Helper()
	silence := media.SilenceFrame(media.LawMu)
	prev := first
	if !bytes.Equal(first.payload, silence) {
		t.Fatalf("seq %d: the barge-in tick sent audio, want silence", first.seq)
	}
	for i := 1; i <= n; i++ {
		a := nextArrival(t, arrivals)
		if a.seq != prev.seq+1 || a.ts != prev.ts+media.FrameSamples {
			t.Fatalf("packet %d after the barge-in: seq %d ts %d follows seq %d ts %d", i, a.seq, a.ts, prev.seq, prev.ts)
		}
		if !bytes.Equal(a.payload, silence) {
			t.Fatalf("packet %d after the barge-in is not silence", i)
		}
		prev = a
	}
}

// wantAudioWithin reads until an audio packet arrives, which shows the pacer
// is back in a state that plays a new utterance.
func wantAudioWithin(t *testing.T, arrivals <-chan arrival, audio []byte, max int) {
	t.Helper()
	for range max {
		if bytes.Equal(nextArrival(t, arrivals).payload, audio) {
			return
		}
	}
	t.Fatalf("no audio within %d packets of a new utterance after the barge-in", max)
}

func TestPacerDoesNotBlockWhenBargeInClearsTxWhilePlaying(t *testing.T) {
	t.Parallel()
	audio := bytes.Repeat([]byte{0x42}, media.FrameSamples)

	// The queue the pacer saw when it decided to dequeue on the barge-in tick.
	for _, count := range []int{1, 2, prebufferFrames, 1500} {
		t.Run(fmt.Sprintf("queued=%d", count), func(t *testing.T) {
			t.Parallel()
			barge := make(chan uint16, 1)
			calls := 0
			session, arrivals, _ := startPacer(t, prebufferFrames, func(r *RTPSession) {
				calls++
				switch calls {
				case 1:
					// Tick 1 starts playout. Leave exactly count frames
					// queued for tick 2, whatever prebufferFrames is.
					r.ClearTx()
					for range count + 1 {
						r.Send(audio)
					}
				case 2:
					// Tick 2 saw queued > 0; empty the queue before it dequeues.
					barge <- r.seq
					r.ClearTx()
				}
			})

			first := waitSeq(t, barge)
			skipped, at := skipTo(t, arrivals, first)
			for _, a := range skipped {
				if !bytes.Equal(a.payload, audio) {
					t.Fatalf("seq %d before the barge-in is not the queued audio", a.seq)
				}
			}
			wantSilenceRun(t, arrivals, at, 10)
			if got := session.Pending(); got != 0 {
				t.Errorf("Pending = %d after the barge-in, want 0", got)
			}
			if _, idle, _ := session.Health(); idle < 11 {
				t.Errorf("idle frames = %d, want at least the 11 silence packets received", idle)
			}
		})
	}
}

func TestPacerDoesNotBlockWhenBargeInClearsTxAtUtteranceStart(t *testing.T) {
	t.Parallel()
	audio := bytes.Repeat([]byte{0x42}, media.FrameSamples)

	t.Run("prebuffer_full", func(t *testing.T) {
		t.Parallel()
		barge := make(chan uint16, 1)
		calls := 0
		_, arrivals, _ := startPacer(t, prebufferFrames, func(r *RTPSession) {
			if calls++; calls == 1 {
				barge <- r.seq
				r.ClearTx()
			}
		})

		skipped, at := skipTo(t, arrivals, waitSeq(t, barge))
		if len(skipped) != 0 {
			t.Fatalf("%d packets before the first dequeue, want none", len(skipped))
		}
		wantSilenceRun(t, arrivals, at, 10)
	})

	t.Run("prebuffer_deadline", func(t *testing.T) {
		t.Parallel()
		barge := make(chan uint16, 1)
		calls := 0
		session, arrivals, _ := startPacer(t, 0, func(r *RTPSession) {
			if calls++; calls == 1 {
				barge <- r.seq
				r.ClearTx()
			}
		})

		// An idle tick seeds the prebuffer deadline; one frame queued after
		// it is below the prebuffer target and is dequeued once the deadline
		// has passed.
		nextArrival(t, arrivals)
		session.Send(audio)

		skipped, at := skipTo(t, arrivals, waitSeq(t, barge))
		for _, a := range skipped {
			if bytes.Equal(a.payload, audio) {
				t.Fatalf("seq %d played the frame before the prebuffer deadline", a.seq)
			}
		}
		wantSilenceRun(t, arrivals, at, 10)

		// The empty dequeue left the pacer waiting for an utterance, not stuck
		// in one, so a new short utterance still plays.
		session.Send(audio)
		wantAudioWithin(t, arrivals, audio, 50)
	})
}

func TestPacerReturnsPromptlyWhenStopRunsDuringTxDequeue(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	session, _, done := startPacer(t, prebufferFrames, func(*RTPSession) {
		if calls++; calls == 1 {
			close(entered)
			<-release
		}
	})

	select {
	case <-entered:
	case <-time.After(pacerWait):
		t.Fatal("the pacer never reached the dequeue")
	}
	session.Stop()
	close(release)

	select {
	case <-done:
	case <-time.After(pacerWait):
		t.Fatal("Run did not return after Stop emptied the queue between the length check and the dequeue")
	}
}

// Seeded barge-ins land on ticks that saw a non-empty queue, bursts arrive in
// step with the packets received, and every frame the queue accepted must
// leave by exactly one path.
func TestPacerSurvivesSeededBargeInsAcrossManyTicks(t *testing.T) {
	t.Parallel()
	const ticks = 200
	audio := bytes.Repeat([]byte{0x42}, media.FrameSamples)
	silence := media.SilenceFrame(media.LawMu)

	plan := rand.New(rand.NewPCG(42, 99))
	bargeAt := make([]bool, ticks+64)
	for i := range bargeAt {
		bargeAt[i] = plan.IntN(4) == 0
	}
	var cleared atomic.Int64
	var barges []uint16 // pacer goroutine until done closes
	calls := 0
	session, arrivals, done := startPacer(t, 0, func(r *RTPSession) {
		calls++
		if bargeAt[calls%len(bargeAt)] {
			barges = append(barges, r.seq)
			cleared.Add(int64(r.ClearTx()))
		}
	})

	feed := rand.New(rand.NewPCG(7, 11))
	accepted := 0
	var got []arrival
	for len(got) < ticks {
		got = append(got, nextArrival(t, arrivals))
		if feed.IntN(2) == 0 {
			for range 1 + feed.IntN(8) {
				if session.Send(audio) {
					accepted++
				}
			}
		}
	}
	// Stop feeding and let the pacer play out or discard what is left. Once Run
	// has returned, every packet it sent is accounted for in Health, and the
	// ones the pacer sent while this goroutine was behind are still in flight.
	for session.Pending() > 0 {
		time.Sleep(FrameDuration)
	}
	// A frame dequeued as the queue reached zero is sent within the next two
	// ticks; Stop before that would close the socket under it.
	settled, _, _ := session.Health()
	for sent, _, _ := session.Health(); sent < settled+2; sent, _, _ = session.Health() {
		time.Sleep(FrameDuration)
	}
	session.Stop()
	<-done
	sent, _, _ := session.Health()
	for int64(len(got)) < sent {
		got = append(got, nextArrival(t, arrivals))
	}

	bySeq := make(map[uint16]arrival, len(got))
	played := 0
	for i, a := range got {
		if i > 0 && (a.seq != got[i-1].seq+1 || a.ts != got[i-1].ts+media.FrameSamples) {
			t.Fatalf("packet %d: seq %d ts %d follows seq %d ts %d, a tick was missed", i, a.seq, a.ts, got[i-1].seq, got[i-1].ts)
		}
		switch {
		case bytes.Equal(a.payload, audio):
			played++
		case !bytes.Equal(a.payload, silence):
			t.Fatalf("seq %d is neither the queued audio nor silence", a.seq)
		}
		bySeq[a.seq] = a
	}
	if len(barges) < 10 || played == 0 {
		t.Fatalf("seed produced %d barge-ins and %d audio packets, want a real mix", len(barges), played)
	}
	for _, seq := range barges {
		a, ok := bySeq[seq]
		if !ok || !bytes.Equal(a.payload, silence) {
			t.Fatalf("the tick whose queue was emptied (seq %d) did not send silence (sent=%v)", seq, ok)
		}
	}
	if want := played + int(cleared.Load()) + session.Pending(); accepted != want {
		t.Errorf("accepted %d frames, but %d played + %d cleared + %d pending = %d", accepted, played, cleared.Load(), session.Pending(), want)
	}
}
