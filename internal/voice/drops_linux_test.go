// SPDX-License-Identifier: Apache-2.0

//go:build linux

package voice

import (
	"net"
	"testing"
	"time"
)

// A socket that is never read overflows at once under a flood, and the
// kernel's drops column is where the loss shows (#597).
func TestSocketDropsCountsTheOverflow(t *testing.T) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The smallest buffer the kernel grants still holds only a datagram or
	// two at a real INVITE's size.
	if err := c.SetReadBuffer(1024); err != nil {
		t.Fatalf("SetReadBuffer: %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	before := socketDrops(port)
	if before < 0 {
		t.Fatalf("no /proc/net/udp row for port %d", port)
	}

	flood, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer flood.Close()
	dgram := make([]byte, 1400)
	for range 500 {
		if _, err := flood.WriteToUDP(dgram, c.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatalf("flood: %v", err)
		}
	}
	// The counter updates as the kernel drops, not on a read; give the last
	// datagrams a moment to land or be dropped.
	for deadline := time.Now().Add(2 * time.Second); ; {
		if socketDrops(port) > before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("drops stayed %d under a 500-datagram flood", socketDrops(port))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSocketDropsHasNoRowForAnUnboundPort(t *testing.T) {
	// A port nobody holds has no row; a free one from the OS, released.
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := socketDrops(port); got != -1 {
		t.Errorf("socketDrops(%d) = %d on an unbound port", port, got)
	}
}
