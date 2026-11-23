// SPDX-License-Identifier: Apache-2.0

package voice

// The #597 measurement: on Linux a burst of INVITEs can overflow the SIP
// socket's kernel receive buffer, and each dropped INVITE or ACK costs the
// call a retransmission. This test measures it for the issue, not for CI:
// it runs only when AICC_MEASURE_SIP_BURST is set, and only on Linux, where
// /proc/net/udp carries a per-socket drops column.
//
//	go test -count=1 -v -run TestMeasureSIPBurst ./internal/voice/
//
// (deliberately without -race: the number wanted is a production build's.)

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rmemMax(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/sys/net/core/rmem_max")
	if err != nil {
		t.Fatalf("rmem_max: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("rmem_max: %v", err)
	}
	return n
}

// burstPeer sends one INVITE and answers it the way a switch does: resend on
// Timer A (500 ms, doubling) until any response arrives, ACK every 200 OK.
// It reports how many times it sent the INVITE and when the final response
// came. A non-200 final response is an outcome, not an error: at burst sizes
// above MaxCalls the 486s are part of the traffic.
func burstPeer(p *peer, start <-chan struct{}, report func(sends int, finalAt time.Time, status int)) {
	<-start
	sends := 0
	buf := make([]byte, 65536)
	send := func() {
		p.request("INVITE", p.offer())
		sends++
	}
	deadline := time.Now().Add(15 * time.Second)
	interval := 500 * time.Millisecond
	next := time.Now().Add(interval)
	send()
	var firstFinal time.Time
	status := 0
	for status == 0 {
		_ = p.sip.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _, err := p.sip.ReadFromUDP(buf)
		if err != nil {
			if time.Now().After(deadline) {
				report(sends, time.Time{}, 0)
				return
			}
			if time.Now().After(next) {
				send()
				interval *= 2
				next = time.Now().Add(interval)
			}
			continue
		}
		msg, err := parseSIP(append([]byte(nil), buf[:n]...))
		if err != nil || msg.statusCode == 0 {
			continue
		}
		if msg.statusCode >= 200 {
			if firstFinal.IsZero() {
				firstFinal = time.Now()
			}
			status = msg.statusCode
			if msg.statusCode == 200 {
				p.request("ACK", "")
			}
			report(sends, firstFinal, status)
			return
		}
	}
}

func TestMeasureSIPBurst(t *testing.T) {
	if os.Getenv("AICC_MEASURE_SIP_BURST") == "" {
		t.Skip("set AICC_MEASURE_SIP_BURST=1 to run the #597 measurement")
	}
	if runtime.GOOS != "linux" {
		t.Skip("the drops column is a Linux /proc/net/udp field")
	}
	rmem := rmemMax(t)
	t.Logf("rmem_default/rmem_max = %d bytes", rmem)

	for _, bufBytes := range []int{0, rmem} {
		for _, burst := range []int{25, 50, 100, 200, 400, 800} {
			uas, _, _ := startUAS(t, func(c *Config) {
				c.RTCPInterval = 0 // RTCP sockets are beside the point here
			})
			if bufBytes > 0 {
				if err := uas.conn.SetReadBuffer(bufBytes); err != nil {
					t.Fatalf("SetReadBuffer(%d): %v", bufBytes, err)
				}
			}
			port := uas.LocalPort()
			before := socketDrops(port)
			if before < 0 {
				t.Fatalf("no /proc/net/udp row for port %d", port)
			}

			peers := make([]*peer, burst)
			for i := range peers {
				peers[i] = newPeer(t, port)
				peers[i].callID = fmt.Sprintf("burst-%d", i)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			var retransmitted, noResponse atomic.Int64
			var lastFinal atomic.Int64
			var sendsTotal atomic.Int64
			statuses := make(chan int, burst)
			for _, p := range peers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					burstPeer(p, start, func(sends int, finalAt time.Time, status int) {
						sendsTotal.Add(int64(sends))
						if sends > 1 {
							retransmitted.Add(1)
						}
						if finalAt.IsZero() {
							noResponse.Add(1)
						} else {
							at := finalAt.UnixNano()
							for {
								old := lastFinal.Load()
								if at <= old || lastFinal.CompareAndSwap(old, at) {
									break
								}
							}
						}
						statuses <- status
					})
				}()
			}
			t0 := time.Now()
			close(start)
			wg.Wait()
			drain := time.Since(t0)
			after := socketDrops(port)
			uas.Stop()

			byStatus := map[int]int{}
			close(statuses)
			for s := range statuses {
				byStatus[s]++
			}
			dropped := after - before
			t.Logf("buffer=%d burst=%d: drops=%d, calls retransmitted=%d (of %d), "+
				"no response in 15s=%d, invites sent=%d, statuses=%v, "+
				"burst drained in %v (≈%.2f ms/invite), last final at +%v",
				bufBytes, burst, dropped, retransmitted.Load(), burst,
				noResponse.Load(), sendsTotal.Load(), byStatus,
				drain, float64(drain.Microseconds())/1000/float64(burst),
				time.Duration(lastFinal.Load())-time.Duration(t0.UnixNano()))
		}
	}
}
