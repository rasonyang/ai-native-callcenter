// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// sdpOffer is the part of an incoming offer this UAS acts on.
//
// The DTMF payload type is whatever the offer said it was. Assuming 101 is the
// bug that makes digits vanish against a peer that numbers telephone-event
// differently, and when the offer carries none, the answer advertises none
// rather than inventing one.
type sdpOffer struct {
	// IP and Port are where the peer wants RTP sent.
	IP   string
	Port int
	// Codecs maps payload type to uppercase codec name.
	Codecs map[int]string
	// DTMFPayloadType is the telephone-event type, or -1 when not offered.
	DTMFPayloadType int
}

// payloadTypeFor finds the offered payload type for a codec name.
func (o *sdpOffer) payloadTypeFor(codec string) (int, bool) {
	for pt, name := range o.Codecs {
		if name == codec {
			return pt, true
		}
	}
	return 0, false
}

// staticPayloadTypes are the RTP/AVP assignments a peer may rely on without
// sending an rtpmap (RFC 3551).
var staticPayloadTypes = map[string]int{"PCMU": 0, "PCMA": 8}

var rtpmapPattern = regexp.MustCompile(`a=rtpmap:(\d+)\s+([\w\-]+)/(\d+)`)

func parseSDP(sdp string) *sdpOffer {
	offer := &sdpOffer{Codecs: map[int]string{}, DTMFPayloadType: -1}

	var audioPayloadTypes []int
	inAudio := false
	for _, line := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "c=") && offer.IP == "":
			if parts := strings.Fields(line); len(parts) >= 3 {
				offer.IP = parts[len(parts)-1]
			}
		case strings.HasPrefix(line, "m=audio"):
			inAudio = true
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				if port, err := strconv.Atoi(parts[1]); err == nil {
					offer.Port = port
				}
				for _, raw := range parts[3:] {
					if pt, err := strconv.Atoi(raw); err == nil {
						audioPayloadTypes = append(audioPayloadTypes, pt)
					}
				}
			}
		case strings.HasPrefix(line, "m="):
			inAudio = false
		case inAudio && strings.HasPrefix(line, "a=rtpmap:"):
			m := rtpmapPattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			pt, _ := strconv.Atoi(m[1])
			name := strings.ToUpper(m[2])
			if name == "TELEPHONE-EVENT" {
				offer.DTMFPayloadType = pt
			} else {
				offer.Codecs[pt] = name
			}
		}
	}

	// Fill in the static assignments the peer left implicit.
	for _, pt := range audioPayloadTypes {
		if _, known := offer.Codecs[pt]; known {
			continue
		}
		for name, static := range staticPayloadTypes {
			if pt == static {
				offer.Codecs[pt] = name
			}
		}
	}
	return offer
}

// negotiate picks a law from the offer following the local preference order.
//
// Both laws are wired end to end — negotiation, SDP, payload type, and the
// companding tables — because a codec that is advertised but only half
// implemented fails at the worst possible moment, on a live call.
func negotiate(offer *sdpOffer, preferences []media.Law) (law media.Law, payloadType int, ok bool) {
	for _, pref := range preferences {
		if pt, found := offer.payloadTypeFor(pref.String()); found {
			return pref, pt, true
		}
	}
	return 0, 0, false
}

// buildSDPAnswer writes the answer carried in the 200 OK.
func buildSDPAnswer(localIP string, localPort int, sessionID uint32,
	law media.Law, payloadType, dtmfPayloadType int) string {

	if payloadType < 0 {
		payloadType = int(law.PayloadType())
	}
	formats := strconv.Itoa(payloadType)
	if dtmfPayloadType >= 0 {
		formats += " " + strconv.Itoa(dtmfPayloadType)
	}

	lines := []string{
		"v=0",
		fmt.Sprintf("o=aicc %d %d IN IP4 %s", sessionID, sessionID, localIP),
		"s=aicc",
		"c=IN IP4 " + localIP,
		"t=0 0",
		fmt.Sprintf("m=audio %d RTP/AVP %s", localPort, formats),
		fmt.Sprintf("a=rtpmap:%d %s/8000", payloadType, law),
	}
	if dtmfPayloadType >= 0 {
		lines = append(lines,
			fmt.Sprintf("a=rtpmap:%d telephone-event/8000", dtmfPayloadType),
			fmt.Sprintf("a=fmtp:%d 0-16", dtmfPayloadType))
	}
	// Stating the packetisation explicitly stops peers from assuming their own
	// default and sending frames this session is not paced for.
	lines = append(lines, "a=ptime:20", "a=sendrecv")

	return strings.Join(lines, "\r\n") + "\r\n"
}
