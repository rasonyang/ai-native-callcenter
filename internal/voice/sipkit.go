// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"net"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This file exports the SIP and SDP building blocks of the UAS unchanged, for
// the test tools that play the other side of a call (internal/calleesim). Each
// function is a thin forward to the unexported one the UAS itself uses, so the
// tools and the application parse and answer SIP the same way. Nothing here
// is called by the application.

// SIPMessage is a parsed SIP request or response.
type SIPMessage struct{ msg *sipMessage }

// ParseSIP parses one datagram.
func ParseSIP(data []byte) (*SIPMessage, error) {
	msg, err := parseSIP(data)
	if err != nil {
		return nil, err
	}
	return &SIPMessage{msg: msg}, nil
}

// Method is the request method, empty for a response or an unsupported method.
func (m *SIPMessage) Method() string { return string(m.msg.method) }

// RequestURI is empty for a response.
func (m *SIPMessage) RequestURI() string { return m.msg.requestURI }

// StatusCode is zero for a request.
func (m *SIPMessage) StatusCode() int { return m.msg.statusCode }

// Header returns the first value of a header, looked up case-insensitively.
func (m *SIPMessage) Header(name string) string { return m.msg.get(name) }

// Headers returns every header's first value, keyed by lowercase name.
func (m *SIPMessage) Headers() map[string]string {
	out := make(map[string]string, len(m.msg.headers))
	for k, v := range m.msg.headers {
		out[k] = v
	}
	return out
}

func (m *SIPMessage) CallID() string  { return m.msg.sipCallID() }
func (m *SIPMessage) From() string    { return m.msg.fromHeader() }
func (m *SIPMessage) To() string      { return m.msg.toHeader() }
func (m *SIPMessage) CSeq() string    { return m.msg.cseq() }
func (m *SIPMessage) Contact() string { return m.msg.contact() }
func (m *SIPMessage) Body() string    { return m.msg.body }

// RecordRoutes returns the Record-Route values in the order received.
func (m *SIPMessage) RecordRoutes() []string { return m.msg.recordRoutes() }

// Routes returns the Route values in the order received.
func (m *SIPMessage) Routes() []string { return m.msg.multi["route"] }

// Response answers the message; see buildResponse for the arguments.
func (m *SIPMessage) Response(status, toTag, cseqOverride, extraHeaders string, body []byte) []byte {
	return buildResponse(m.msg, status, toTag, cseqOverride, extraHeaders, body)
}

// Response487 terminates the INVITE this message (a CANCEL) cancelled.
func (m *SIPMessage) Response487(toTag string) []byte { return build487(m.msg, toTag) }

// URIOfHeader extracts the sip: URI from a From, To or Contact value.
func URIOfHeader(header string) string { return extractURI(header) }

// SDPOffer is the part of an SDP body the UAS acts on.
type SDPOffer = sdpOffer

// ParseSDP reads an offer.
func ParseSDP(sdp string) *SDPOffer { return parseSDP(sdp) }

// NegotiateCodec picks a law from the offer in the order of preferences.
func NegotiateCodec(offer *SDPOffer, preferences []media.Law) (media.Law, int, bool) {
	return negotiate(offer, preferences)
}

// BuildSDPAnswer writes the SDP carried in a 200 OK.
func BuildSDPAnswer(localIP string, localPort int, sessionID uint32,
	law media.Law, payloadType, dtmfPayloadType int) string {
	return buildSDPAnswer(localIP, localPort, sessionID, law, payloadType, dtmfPayloadType)
}

// BuildBye renders a BYE for a dialog this side accepted, with no Reason.
// routeSet is the Record-Route set as the INVITE carried it, in order
// (RFC 3261 §12.1.1). The BYE's Route headers keep that order; a strict router
// first in the set (no `;lr`) takes the Request-URI and the remote Contact
// becomes the last Route (§12.2.1.1). Each BYE has a fresh Via branch and
// Max-Forwards: 70. Send it to RouteSetNextHop when routeSet is not empty.
func BuildBye(callID, fromHeader, toHeader, localTag, localIP string,
	localPort int, routeSet []string, remoteContact string) []byte {
	return buildBye(callID, fromHeader, toHeader, localTag, localIP, localPort,
		routeSet, remoteContact, byeReason{})
}

// RouteSetNextHop is the UDP address an in-dialog request is sent to when the
// dialog has a route set: the first entry's URI, by the rule the UAS's own BYE
// follows. It fails for an empty set, a sips: or non-UDP entry, and a host
// that is not an IP literal; nothing is resolved.
func RouteSetNextHop(routeSet []string) (*net.UDPAddr, error) {
	return routeSetNextHop(routeSet, "udp")
}

// UDPAddrOfURI is the UDP address a sip: URI sends a request to, by the same
// rules as RouteSetNextHop: an IP literal host and its port (5060 when absent,
// maddr when present), refused for a hostname, sips: and a transport other
// than UDP.
func UDPAddrOfURI(uri string) (*net.UDPAddr, error) {
	hop, err := parseHop(uri)
	if err != nil {
		return nil, err
	}
	return hop.udpAddr("udp")
}
