// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
)

// RTCP here exists for one reason: a receive timeout detects that we have
// stopped hearing the far end, but nothing local can detect that the far end
// has stopped hearing us. The peer's reception reports are the only way to see
// a one-way media failure, which on a phone call is indistinguishable to the
// caller from the bot having hung up.
//
// It is deliberately minimal — a sender report with one reception report and a
// CNAME, every few seconds. No SRTP, no bandwidth adaptation. A peer that
// sends no RTCP costs nothing; the stats simply stay empty.

// DefaultRTCPInterval is how often a compound report goes out.
const DefaultRTCPInterval = 5 * time.Second

// receptionStats accumulates what a reception report needs. The RTP receive
// goroutine writes it and the reporting goroutine reads it.
type receptionStats struct {
	mu          sync.Mutex
	initialized bool
	remoteSSRC  uint32
	baseSeq     uint16
	maxSeq      uint16
	cycles      uint32 // wrap count, already shifted into the high half
	received    uint32

	// Snapshots from the previous report, for the fraction-lost calculation.
	expectedPrior uint32
	receivedPrior uint32

	// Interarrival jitter in RTP timestamp units (RFC 3550 §6.4.1).
	transit int32
	jitter  float64
}

func (s *receptionStats) onPacket(seq uint16, timestamp, ssrc uint32) {
	// Arrival time expressed in the stream's own 8 kHz units, so it is
	// directly comparable with the RTP timestamp.
	arrival := uint32(time.Now().UnixNano() / int64(time.Second/8000))

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.initialized {
		s.initialized = true
		s.remoteSSRC = ssrc
		s.baseSeq, s.maxSeq = seq, seq
		s.received = 1
		s.transit = int32(arrival - timestamp)
		return
	}

	s.remoteSSRC = ssrc
	s.received++
	if seqDelta(seq, s.maxSeq) > 0 {
		if seq < s.maxSeq {
			s.cycles += 1 << 16
		}
		s.maxSeq = seq
	}

	transit := int32(arrival - timestamp)
	drift := transit - s.transit
	s.transit = transit
	if drift < 0 {
		drift = -drift
	}
	s.jitter += (float64(drift) - s.jitter) / 16
}

// report builds a reception report and rolls the interval snapshot. It
// reports false until the first packet has arrived.
func (s *receptionStats) report() (rtcp.ReceptionReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialized {
		return rtcp.ReceptionReport{}, false
	}

	extended := s.cycles | uint32(s.maxSeq)
	expected := extended - uint32(s.baseSeq) + 1
	var totalLost uint32
	if expected > s.received {
		totalLost = expected - s.received
	}

	expectedInterval := expected - s.expectedPrior
	receivedInterval := s.received - s.receivedPrior
	s.expectedPrior, s.receivedPrior = expected, s.received

	var fraction uint8
	if expectedInterval > receivedInterval {
		fraction = uint8((expectedInterval - receivedInterval) * 256 / expectedInterval)
	}

	return rtcp.ReceptionReport{
		SSRC:               s.remoteSSRC,
		FractionLost:       fraction,
		TotalLost:          totalLost & 0x00FFFFFF,
		LastSequenceNumber: extended,
		Jitter:             uint32(s.jitter),
	}, true
}

// RemoteQuality is what the peer says about the audio we are sending it.
type RemoteQuality struct {
	FractionLost uint8
	TotalLost    uint32
	Jitter       uint32
	// At is zero until the peer has reported at all.
	At time.Time
}

// RTCPSession reports on, and listens about, one RTP session.
type RTCPSession struct {
	rtp      *RTPSession
	conn     *net.UDPConn
	remote   *net.UDPAddr
	interval time.Duration
	cname    string
	log      *slog.Logger

	running   atomic.Bool
	closeOnce sync.Once

	mu      sync.Mutex
	quality RemoteQuality
}

// NewRTCPSession binds the RTP port plus one and reports to the peer's RTP
// port plus one, which is the convention every switch this talks to follows.
func NewRTCPSession(session *RTPSession, remoteRTP *net.UDPAddr,
	interval time.Duration, log *slog.Logger) (*RTCPSession, error) {

	conn, err := net.ListenUDP("udp4",
		&net.UDPAddr{IP: net.IPv4zero, Port: session.LocalPort + 1})
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		interval = DefaultRTCPInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &RTCPSession{
		rtp:      session,
		conn:     conn,
		remote:   &net.UDPAddr{IP: remoteRTP.IP, Port: remoteRTP.Port + 1},
		interval: interval,
		cname:    "aicc",
		log:      log,
	}, nil
}

func (s *RTCPSession) Start() {
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	go s.receiveLoop()
	go s.reportLoop()
}

// Stop is safe on a session that was never started — the socket is bound by
// the constructor, so it has to be released either way.
func (s *RTCPSession) Stop() {
	if s.running.CompareAndSwap(true, false) {
		s.sendGoodbye()
	}
	s.closeOnce.Do(func() { _ = s.conn.Close() })
}

// Quality returns the peer's most recent report on our outbound stream.
func (s *RTCPSession) Quality() RemoteQuality {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quality
}

func (s *RTCPSession) reportLoop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for s.running.Load() {
		<-ticker.C
		if !s.running.Load() {
			return
		}
		s.sendReport()
	}
}

func (s *RTCPSession) sendReport() {
	senderReport := &rtcp.SenderReport{
		SSRC:        s.rtp.ssrc.Load(),
		NTPTime:     ntpTimestamp(time.Now()),
		RTPTime:     s.rtp.timestamp.Load(),
		PacketCount: uint32(s.rtp.sent.Load()),
		OctetCount:  uint32(s.rtp.sentOctets.Load()),
	}
	if rr, ok := s.rtp.rxStats.report(); ok {
		senderReport.Reports = []rtcp.ReceptionReport{rr}
	}
	description := &rtcp.SourceDescription{Chunks: []rtcp.SourceDescriptionChunk{{
		Source: s.rtp.ssrc.Load(),
		Items:  []rtcp.SourceDescriptionItem{{Type: rtcp.SDESCNAME, Text: s.cname}},
	}}}

	payload, err := rtcp.Marshal([]rtcp.Packet{senderReport, description})
	if err != nil {
		return
	}
	_, _ = s.conn.WriteToUDP(payload, s.remote)
}

func (s *RTCPSession) sendGoodbye() {
	bye := &rtcp.Goodbye{Sources: []uint32{s.rtp.ssrc.Load()}}
	if payload, err := rtcp.Marshal([]rtcp.Packet{bye}); err == nil {
		_, _ = s.conn.WriteToUDP(payload, s.remote)
	}
}

func (s *RTCPSession) receiveLoop() {
	buf := make([]byte, 4096)
	for s.running.Load() {
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, packet := range packets {
			s.handlePacket(packet)
		}
	}
}

func (s *RTCPSession) handlePacket(packet rtcp.Packet) {
	var reports []rtcp.ReceptionReport
	switch p := packet.(type) {
	case *rtcp.SenderReport:
		reports = p.Reports
	case *rtcp.ReceiverReport:
		reports = p.Reports
	case *rtcp.Goodbye:
		s.log.Info("rtcp goodbye from remote", "sources", p.Sources)
		return
	default:
		return
	}

	for _, rr := range reports {
		if rr.SSRC != s.rtp.ssrc.Load() {
			continue // a report about some other stream
		}
		s.mu.Lock()
		s.quality = RemoteQuality{
			FractionLost: rr.FractionLost,
			TotalLost:    rr.TotalLost,
			Jitter:       rr.Jitter,
			At:           time.Now(),
		}
		s.mu.Unlock()

		// A quarter of our audio going missing is a broken call, and it is
		// invisible from this side without the report.
		if rr.FractionLost >= 64 {
			s.log.Warn("remote reports loss on our outbound audio",
				"fraction", float64(rr.FractionLost)/256,
				"totalLost", rr.TotalLost, "jitter", rr.Jitter)
		}
	}
}

// ntpTimestamp is the 64-bit NTP form RFC 3550 wants: seconds since 1900 in
// the high half, fraction in the low half.
func ntpTimestamp(t time.Time) uint64 {
	const secondsFrom1900To1970 = 2208988800
	seconds := uint64(t.Unix()) + secondsFrom1900To1970
	fraction := uint64(t.Nanosecond()) << 32 / uint64(time.Second)
	return seconds<<32 | fraction
}
