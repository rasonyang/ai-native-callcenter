// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"net"
	"testing"
)

// callSockets returns the local addresses of an answered call's RTP and RTCP
// sockets.
func callSockets(t *testing.T, d *Dialog) (rtp, rtcp *net.UDPAddr) {
	t.Helper()
	if d.RTP.conn == nil {
		t.Fatal("the call has no RTP socket")
	}
	d.mu.Lock()
	session := d.rtcp
	d.mu.Unlock()
	if session == nil {
		t.Fatal("the call has no RTCP socket")
	}
	return d.RTP.conn.LocalAddr().(*net.UDPAddr), session.conn.LocalAddr().(*net.UDPAddr)
}

// assertCallOn checks that the SDP answer names want and that the call's RTP
// and RTCP sockets are bound to exactly that address.
func assertCallOn(t *testing.T, answer *sipMessage, d *Dialog, want string) {
	t.Helper()
	if got := parseSDP(answer.body).IP; got != want {
		t.Errorf("SDP answer names %s, want %s", got, want)
	}
	if d.LocalIP != want {
		t.Errorf("dialog LocalIP = %s, want %s", d.LocalIP, want)
	}
	rtp, rtcp := callSockets(t, d)
	if rtp.IP.String() != want {
		t.Errorf("RTP bound %s, want %s", rtp, want)
	}
	if rtcp.IP.String() != want {
		t.Errorf("RTCP bound %s, want %s", rtcp, want)
	}
	if rtcp.Port != rtp.Port+1 {
		t.Errorf("RTCP port %d is not RTP port %d plus one", rtcp.Port, rtp.Port)
	}
}

func TestACallToALoopbackListenerIsAnsweredAndBoundOnLoopback(t *testing.T) {
	_, hooks, p := startUAS(t, nil)
	answer, dialog := p.connect(hooks)

	assertCallOn(t, answer, dialog, "127.0.0.1")
}

func TestACallToAWildcardListenerIsAnsweredAndBoundOnTheAddressItWasDialled(t *testing.T) {
	_, hooks, p := startUAS(t, func(c *Config) { c.SIPHost = "0.0.0.0" })
	answer, dialog := p.connect(hooks)

	assertCallOn(t, answer, dialog, "127.0.0.1")
}

func TestACallToAWildcardListenerFollowsTheDestinationOfEachInvite(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot list interface addresses: %v", err)
	}
	own := ""
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				own = ip.String()
				break
			}
		}
	}
	if own == "" {
		t.Skip("this host has no non-loopback IPv4 address")
	}

	_, hooks, p := startUAS(t, func(c *Config) { c.SIPHost = "0.0.0.0" })
	// The switch signals from, and is dialled at, the host's own address.
	sip, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(own)})
	if err != nil {
		t.Skipf("cannot bind %s: %v", own, err)
	}
	t.Cleanup(func() { sip.Close() })
	p.sip = sip
	p.uasAddr = &net.UDPAddr{IP: net.ParseIP(own), Port: p.uasAddr.Port}

	answer, dialog := p.connect(hooks)

	assertCallOn(t, answer, dialog, own)
}
