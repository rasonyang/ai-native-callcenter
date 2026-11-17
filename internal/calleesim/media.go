// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// startMediaLocked opens the RTP path once, for early media or for the answer,
// whichever comes first. d.mu is held.
func (d *dialog) startMediaLocked(offer *voice.SDPOffer, payloadType int) error {
	if d.rtp != nil {
		return nil
	}
	if offer.Port == 0 || offer.IP == "" {
		return fmt.Errorf("offer has no media address (%q:%d)", offer.IP, offer.Port)
	}
	remote, err := net.ResolveUDPAddr("udp", net.JoinHostPort(offer.IP, fmt.Sprint(offer.Port)))
	if err != nil {
		return err
	}
	session, err := d.s.openRTP(d.law, offer.DTMFPayloadType, remote)
	if err != nil {
		return err
	}
	d.rtp = session
	d.localRTPPort = session.LocalPort

	go session.Run()
	go d.receive(session)
	go d.feed(session)
	return nil
}

// openRTP binds a media port, within the configured range when there is one.
func (s *Server) openRTP(law media.Law, dtmfPayloadType int, remote *net.UDPAddr) (*voice.RTPSession, error) {
	if s.cfg.RTPPortMin <= 0 {
		session := voice.NewRTPSession(0, law, dtmfPayloadType, s.log)
		return session, session.Start(remote)
	}
	var last error
	for port := s.cfg.RTPPortMin; port <= s.cfg.RTPPortMax; port += 2 {
		session := voice.NewRTPSession(port, law, dtmfPayloadType, s.log)
		if last = session.Start(remote); last == nil {
			return session, nil
		}
	}
	return nil, fmt.Errorf("no free rtp port in %d-%d: %w", s.cfg.RTPPortMin, s.cfg.RTPPortMax, last)
}

// sdpLocked is the answer carried by the 183 with early media and the 200 OK,
// the same in both so the media path does not move.
func (d *dialog) sdpLocked(payloadType int, offer *voice.SDPOffer) string {
	return voice.BuildSDPAnswer(d.localIP, d.localRTPPort, uint32(d.rec.InviteAt.Unix()),
		d.law, payloadType, offer.DTMFPayloadType)
}

// receive measures the caller's audio until the dialog ends.
func (d *dialog) receive(session *voice.RTPSession) {
	for {
		select {
		case <-d.ctx.Done():
			return
		case frame := <-session.Frames():
			at := time.Now()
			rms := frameRMS(d.law, frame)
			media.PutBytes(frame)

			d.mu.Lock()
			d.rec.Downlink.addFrame(at.Sub(d.rec.InviteAt), rms)
			if rms > silenceRMS {
				d.rec.Downlink.addAudio(monotonicMs(at, d.inviteAt))
			}
			if rms > silenceRMS && !d.answeredAt.IsZero() && d.rec.Downlink.FirstAudioAfterAnswerMs == nil {
				first := float64(at.Sub(d.answeredAt).Microseconds()) / 1000
				d.rec.Downlink.FirstAudioAfterAnswerMs = &first
			}
			d.mu.Unlock()
		}
	}
}

// feed paces the behaviour's uplink onto the session. Silence queues nothing:
// the session sends silent frames by itself when it has none.
func (d *dialog) feed(session *voice.RTPSession) {
	var source audioSource
	started := time.Now()
	d.mu.Lock()
	startMs := monotonicMs(started, d.inviteAt)
	d.rec.UplinkStartedAfterInviteMs = &startMs
	d.mu.Unlock()
	switch d.behaviour.Uplink.Kind {
	case UplinkBursts:
		// Timed on the clock, not by counting ticks: a tick the runtime
		// dropped must not move a burst.
		bursts := newBurstSource(d.law, d.behaviour.Uplink.FrequencyHz, d.behaviour.Uplink.Bursts)
		ticker := time.NewTicker(voice.FrameDuration)
		defer ticker.Stop()
		for {
			select {
			case <-d.ctx.Done():
				return
			case <-ticker.C:
				if frame := bursts.frameAt(time.Since(started)); frame != nil {
					session.Send(frame)
				}
			}
		}
	case UplinkTone:
		source = newToneSource(d.law, d.behaviour.Uplink.FrequencyHz)
	case UplinkWAV:
		source = &pcmSource{law: d.law, samples: d.s.wav[d.behaviour.Uplink.File]}
	default:
		return
	}
	ticker := time.NewTicker(voice.FrameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if frame := source.next(); frame != nil {
				session.Send(frame)
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
