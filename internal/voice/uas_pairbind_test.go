// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The UAS binds a call's RTP port (and RTCP port beside it) before it answers,
// so the 200 OK never names a port another process holds (#450). These tests
// hold real sockets and check what the UAS does around them.

var loopback = net.IPv4(127, 0, 0, 1)

// freeWindow returns an even base port such that base..base+n-1 all bind on
// loopback right now. Windows sit in 30000-31999, clear of the default range
// the other packages' tests use and of the ephemeral range.
func freeWindow(t *testing.T, n int) int {
	t.Helper()
	for range 500 {
		base := 30000 + rand.IntN((2000-n)/2)*2
		var conns []*net.UDPConn
		ok := true
		for p := base; p < base+n; p++ {
			c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: p})
			if err != nil {
				ok = false
				break
			}
			conns = append(conns, c)
		}
		for _, c := range conns {
			c.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatalf("no free window of %d ports", n)
	return 0
}

// freePairBase is freeWindow for n pairs.
func freePairBase(t *testing.T, n int) int { return freeWindow(t, 2*n) }

func holdUDP(t *testing.T, ip net.IP, port int) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		t.Fatalf("hold %v:%d: %v", ip, port, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// canBind reports whether a test can take ip:port now, i.e. nobody holds it.
func canBind(port int) bool {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: port})
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func usedPortCount(u *UAS) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.usedPorts)
}

type bindRig struct {
	uas   *UAS
	hooks *callHooks
	p     *peer
	logs  *logCapture
	slots *BotSlots
}

func startBindRig(t *testing.T, low, high int, rtcp bool, adjust ...func(*Config)) *bindRig {
	t.Helper()
	r := &bindRig{logs: newLogCapture(), slots: slotsOf(t, 4, 0)}
	r.uas, r.hooks, r.p = startUAS(t, func(c *Config) {
		r.logs.configure(c)
		c.RTPPortRange = [2]int{low, high}
		c.Slots = r.slots
		if !rtcp {
			c.RTCPInterval = 0
		}
		for _, f := range adjust {
			f(c)
		}
	})
	return r
}

// invite sends an INVITE from a fresh peer and returns the first final status.
func (r *bindRig) invite(t *testing.T, id string) (*peer, *sipMessage) {
	t.Helper()
	p := newPeer(t, r.p.uasAddr.Port)
	p.callID = id
	p.request("INVITE", p.offer())
	for {
		msg := p.await(2 * time.Second)
		if msg.statusCode >= 200 {
			return p, msg
		}
	}
}

func answerPort(t *testing.T, msg *sipMessage) int {
	t.Helper()
	sdp := parseSDP(msg.body)
	if sdp.Port == 0 {
		t.Fatalf("200 OK carries no media port: %q", msg.body)
	}
	return sdp.Port
}

func (r *bindRig) assertNoBindFailureLogged(t *testing.T) {
	t.Helper()
	for _, rec := range r.logs.records(t) {
		if rec.msg() == "media failed to start" {
			t.Errorf("a call failed to start after its ACK: %s", rec.raw)
		}
	}
}

func (r *bindRig) skipped(t *testing.T) []logRecord {
	t.Helper()
	var out []logRecord
	for _, rec := range r.logs.records(t) {
		if strings.HasPrefix(rec.msg(), "rtp port pair skipped") {
			out = append(out, rec)
		}
	}
	return out
}

// answered completes a call on an answered INVITE and returns its dialog.
func (r *bindRig) answered(t *testing.T, p *peer, ans *sipMessage) *Dialog {
	t.Helper()
	p.request("ACK", "")
	select {
	case d := <-r.hooks.started:
		return d
	case err := <-r.hooks.failed:
		t.Fatalf("call failed to start: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the call never started")
	}
	return nil
}

func TestAHeldSpecificAddressSkipsToTheNextPair(t *testing.T) {
	base := freePairBase(t, 2)
	holdUDP(t, loopback, base) // the UAS's own address, first pair's RTP port
	r := startBindRig(t, base, base+3, true)

	// Whichever pair the random pick tries first, the held one is never answered.
	for i := range 1 {
		p, ans := r.invite(t, fmt.Sprintf("held-ip-%d", i))
		if ans.statusCode != 200 {
			t.Fatalf("status %d, want 200 on the free pair", ans.statusCode)
		}
		if got := answerPort(t, ans); got != base+2 {
			t.Fatalf("answered port %d, want %d", got, base+2)
		}
		d := r.answered(t, p, ans)
		if canBind(base + 2) {
			t.Error("the answered RTP port is not held by the UAS")
		}
		if canBind(base + 3) {
			t.Error("the answered pair's RTCP port is not held by the UAS")
		}
		if d.RTP.LocalPort != base+2 {
			t.Errorf("session port %d, SDP port %d", d.RTP.LocalPort, base+2)
		}
	}
	r.assertNoBindFailureLogged(t)
}

func TestAHeldWildcardAddressBlocksThePair(t *testing.T) {
	base := freePairBase(t, 2)
	holdUDP(t, net.IPv4zero, base)
	r := startBindRig(t, base, base+3, true)

	p, ans := r.invite(t, "held-wildcard")
	if ans.statusCode != 200 || answerPort(t, ans) != base+2 {
		t.Fatalf("status %d port %d, want 200 on %d", ans.statusCode, answerPort(t, ans), base+2)
	}
	r.answered(t, p, ans)
	r.assertNoBindFailureLogged(t)
}

// A holder on another local IP does not block a bind on the UAS's address on
// macOS or Linux (measured for #450): the call proceeds on the very pair the
// holder sits on. The test records whether this host agrees.
func TestAHolderOnAnotherLocalAddressDoesNotBlock(t *testing.T) {
	var other net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			other = ipn.IP.To4()
			break
		}
	}
	if other == nil {
		t.Skip("no non-loopback IPv4 address on this host")
	}
	base := freePairBase(t, 1)
	holder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: other, Port: base})
	if err != nil {
		t.Skipf("cannot hold %v:%d: %v", other, base, err)
	}
	t.Cleanup(func() { holder.Close() })
	t.Logf("holder on %v:%d; one-pair range on 127.0.0.1", other, base)
	r := startBindRig(t, base, base+1, true)

	p, ans := r.invite(t, "other-ip")
	if ans.statusCode != 200 || answerPort(t, ans) != base {
		t.Fatalf("status %d: a holder on another local address blocked the bind on this OS", ans.statusCode)
	}
	t.Logf("the call proceeded on %d although %v:%d is held: another local address does not block", base, other, base)
	r.answered(t, p, ans)
}

func TestAHeldRTCPPortSkipsThePair(t *testing.T) {
	base := freePairBase(t, 2)
	holdUDP(t, loopback, base+1) // only P+1 of the first pair
	r := startBindRig(t, base, base+3, true)

	p, ans := r.invite(t, "held-rtcp")
	if ans.statusCode != 200 || answerPort(t, ans) != base+2 {
		t.Fatalf("status %d port %d, want 200 on %d", ans.statusCode, answerPort(t, ans), base+2)
	}
	r.answered(t, p, ans)
	// The pair that was refused bound its RTP half first; it must be given back.
	if !canBind(base) {
		t.Error("the refused pair's RTP socket leaked")
	}
	r.assertNoBindFailureLogged(t)
}

// With RTCP off only the RTP port matters.
func TestAHeldRTCPPortDoesNotMatterWhenRTCPIsOff(t *testing.T) {
	base := freePairBase(t, 1)
	holdUDP(t, loopback, base+1)
	r := startBindRig(t, base, base+1, false)

	p, ans := r.invite(t, "rtcp-off")
	if ans.statusCode != 200 || answerPort(t, ans) != base {
		t.Fatalf("status %d, want 200 on %d", ans.statusCode, base)
	}
	r.answered(t, p, ans)
}

func TestAllPairsHeldIsA503WithTheSlotReleased(t *testing.T) {
	base := freePairBase(t, 2)
	holders := []*net.UDPConn{holdUDP(t, loopback, base), holdUDP(t, loopback, base+2)}
	r := startBindRig(t, base, base+3, true)

	_, ans := r.invite(t, "all-held")
	if ans.statusCode != 503 {
		t.Fatalf("status %d, want 503", ans.statusCode)
	}
	if ans.body != "" {
		t.Errorf("a refusal carried a body: %q", ans.body)
	}
	waitFor(t, "the refused call to give its slot back", func() bool { return r.slots.InUse() == 0 })
	if got := usedPortCount(r.uas); got != 0 {
		t.Errorf("%d ports still claimed after the refusal", got)
	}
	if got := r.uas.ActiveCalls(); got != 0 {
		t.Errorf("%d active calls after the refusal", got)
	}
	// Each refused pair is named once, with the port and the Call-ID.
	skipped := r.skipped(t)
	if len(skipped) != 2 {
		t.Fatalf("%d skipped-pair lines, want 2: %v", len(skipped), skipped)
	}
	for _, rec := range skipped {
		if rec.attrs["sipCallId"] != "all-held" || rec.attrs["rtpPort"] == nil || rec.attrs["error"] == nil {
			t.Errorf("skipped-pair line lacks sipCallId, rtpPort or error: %s", rec.raw)
		}
	}

	// A holder that closes partway: the next call gets the pair.
	holders[1].Close()
	_, ans = r.invite(t, "after-release")
	if ans.statusCode != 200 || answerPort(t, ans) != base+2 {
		t.Fatalf("status %d, want 200 on %d after the holder closed", ans.statusCode, base+2)
	}
}

func TestATCPListenerDoesNotBlockTheUDPPair(t *testing.T) {
	base := freePairBase(t, 1)
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", base))
	if err != nil {
		t.Skipf("tcp port %d busy: %v", base, err)
	}
	t.Cleanup(func() { ln.Close() })
	r := startBindRig(t, base, base+1, true)

	p, ans := r.invite(t, "tcp-holder")
	if ans.statusCode != 200 || answerPort(t, ans) != base {
		t.Fatalf("status %d, want 200 on %d", ans.statusCode, base)
	}
	r.answered(t, p, ans)
}

func TestRangeEdges(t *testing.T) {
	t.Run("odd low bound rounds up", func(t *testing.T) {
		base := freePairBase(t, 2)
		r := startBindRig(t, base+1, base+4, true) // usable pair: base+2, base+3
		p, ans := r.invite(t, "odd-low")
		if ans.statusCode != 200 || answerPort(t, ans) != base+2 {
			t.Fatalf("status %d, want 200 on %d", ans.statusCode, base+2)
		}
		r.answered(t, p, ans)
	})
	t.Run("one pair range", func(t *testing.T) {
		base := freePairBase(t, 1)
		r := startBindRig(t, base, base+1, true)
		p, ans := r.invite(t, "one-pair")
		if ans.statusCode != 200 || answerPort(t, ans) != base {
			t.Fatalf("status %d, want 200 on %d", ans.statusCode, base)
		}
		// The only pair is taken: the next call has none.
		_, second := r.invite(t, "one-pair-2")
		if second.statusCode != 503 {
			t.Fatalf("second call status %d, want 503", second.statusCode)
		}
		r.answered(t, p, ans)
	})
	t.Run("high bound equals P+1", func(t *testing.T) {
		base := freePairBase(t, 2)
		holdUDP(t, loopback, base)
		r := startBindRig(t, base, base+3, true)
		p, ans := r.invite(t, "high-edge")
		if ans.statusCode != 200 || answerPort(t, ans) != base+2 {
			t.Fatalf("status %d, want 200 on %d", ans.statusCode, base+2)
		}
		r.answered(t, p, ans)
		if canBind(base+3) || canBind(base+2) {
			t.Error("the pair at the top of the range is not held")
		}
	})
}

// A holder in a child process, the way the switch holds a port.
func TestAHolderInAnotherProcessIsSkipped(t *testing.T) {
	base := freePairBase(t, 2)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHolderHelperProcess$")
	cmd.Env = append(os.Environ(), fmt.Sprintf("AICC_TEST_HOLD_UDP=%d", base))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Kill rather than wait for the helper to see EOF: the child is a
		// test binary and may be slow to start or stop under load. Its stdin
		// closing still ends it if this process dies first.
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not come up: %q %v", line, err)
	}

	r := startBindRig(t, base, base+3, true)
	p, ans := r.invite(t, "child-holder")
	if ans.statusCode != 200 || answerPort(t, ans) != base+2 {
		t.Fatalf("status %d, want 200 on %d", ans.statusCode, base+2)
	}
	r.answered(t, p, ans)
}

// TestHolderHelperProcess is not a test: the child of the one above. It holds
// 0.0.0.0:$AICC_TEST_HOLD_UDP until stdin closes.
func TestHolderHelperProcess(t *testing.T) {
	var port int
	if _, err := fmt.Sscanf(os.Getenv("AICC_TEST_HOLD_UDP"), "%d", &port); err != nil {
		t.Skip("helper process only")
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	defer c.Close()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// A reserved slot comes back when the pair cannot be bound, and the call counts
// end where they started.
func TestAReservedCallWithNoPairGivesItsSlotBack(t *testing.T) {
	base := freePairBase(t, 1)
	holdUDP(t, loopback, base)
	r := startBindRig(t, base, base+1, true)
	if !r.slots.Reserve(peerReservationKey) {
		t.Fatal("setup")
	}
	if r.slots.Reserved() != 1 {
		t.Fatal("setup: nothing reserved")
	}

	_, ans := r.invite(t, "reserved-refused")
	if ans.statusCode != 503 {
		t.Fatalf("status %d, want 503", ans.statusCode)
	}
	waitFor(t, "the slot to come back", func() bool {
		return r.slots.InUse() == 0 && r.slots.Reserved() == 0
	})
	if r.uas.ActiveCalls() != 0 || usedPortCount(r.uas) != 0 {
		t.Errorf("active %d, claimed ports %d", r.uas.ActiveCalls(), usedPortCount(r.uas))
	}
	// The slot is genuinely free: a full set of other calls fits.
	for i := range 4 {
		if !r.slots.TryAcquire() {
			t.Fatalf("slot %d of 4 is not available", i)
		}
	}
}

// Every way a call can end releases both sockets.
func TestNoPathLeaksTheBoundPair(t *testing.T) {
	t.Run("ack timeout", func(t *testing.T) {
		base := freePairBase(t, 1)
		r := startBindRig(t, base, base+1, true, func(c *Config) { c.AckTimeout = 50 * time.Millisecond })
		_, ans := r.invite(t, "leak-ack")
		if ans.statusCode != 200 {
			t.Fatalf("status %d", ans.statusCode)
		}
		waitFor(t, "the ack timeout to release the pair", func() bool { return canBind(base) && canBind(base+1) })
		waitFor(t, "the slot", func() bool { return r.slots.InUse() == 0 })
		waitFor(t, "the ports to be unclaimed", func() bool { return usedPortCount(r.uas) == 0 })
	})
	t.Run("cancel", func(t *testing.T) {
		base := freePairBase(t, 1)
		r := startBindRig(t, base, base+1, true)
		p, ans := r.invite(t, "leak-cancel")
		if ans.statusCode != 200 {
			t.Fatalf("status %d", ans.statusCode)
		}
		p.request("CANCEL", "")
		waitFor(t, "cancel to release the pair", func() bool { return canBind(base) && canBind(base+1) })
	})
	t.Run("hangup", func(t *testing.T) {
		base := freePairBase(t, 1)
		r := startBindRig(t, base, base+1, true)
		p, ans := r.invite(t, "leak-bye")
		d := r.answered(t, p, ans)
		d.Stop()
		waitFor(t, "the call to end", func() bool { return r.uas.ActiveCalls() == 0 })
		waitFor(t, "the pair to be released", func() bool { return canBind(base) && canBind(base+1) })
		waitFor(t, "the ports to be unclaimed", func() bool { return usedPortCount(r.uas) == 0 })
	})
	t.Run("uas stop", func(t *testing.T) {
		base := freePairBase(t, 1)
		r := startBindRig(t, base, base+1, true)
		_, ans := r.invite(t, "leak-stop") // answered, never acked
		if ans.statusCode != 200 {
			t.Fatalf("status %d", ans.statusCode)
		}
		r.uas.Stop()
		if !canBind(base) || !canBind(base+1) {
			t.Error("UAS.Stop left the pair bound")
		}
	})
}

// 200 INVITEs at once against a range where a third of the pairs are held by
// someone else. Every 200 OK names a socket the UAS holds and no call fails
// after its ACK.
func TestConcurrentInvitesNeverAnswerWithAForeignPort(t *testing.T) {
	const calls, pairs = 200, 360
	base := freeWindow(t, 2*pairs)
	held := map[int]bool{}
	for i := 0; i < pairs; i += 3 {
		holdUDP(t, loopback, base+2*i) // RTP half of every third pair
		held[base+2*i] = true
	}

	logs := newLogCapture()
	var started, failed atomic.Int32
	cfg := DefaultConfig()
	cfg.SIPHost = "127.0.0.1"
	cfg.SIPPort = 0
	logs.configure(&cfg)
	cfg.RTPPortRange = [2]int{base, base + 2*pairs - 1}
	cfg.RTPDeadTimeout, cfg.FirstMediaTimeout = 0, 0
	// A starved runner can take longer than the default 3 s to ACK; an ACK
	// timeout would free a pair mid-test and look like a duplicate answer.
	cfg.AckTimeout = time.Minute
	uas := NewUAS(cfg)
	uas.OnCallStarted = func(*Dialog) { started.Add(1) }
	uas.OnCallFailed = func(*Dialog, error) { failed.Add(1) }
	if err := uas.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(uas.Stop)

	var mu sync.Mutex
	ports := map[int]bool{}
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := range calls {
		p := newPeer(t, uas.LocalPort())
		p.callID = fmt.Sprintf("conc-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.request("INVITE", p.offer())
			for {
				msg, err := p.read(10 * time.Second)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", p.callID, err)
					return
				}
				if msg.statusCode < 200 {
					continue
				}
				if msg.statusCode != 200 {
					errs <- fmt.Errorf("%s: status %d", p.callID, msg.statusCode)
					return
				}
				port := parseSDP(msg.body).Port
				mu.Lock()
				dup := ports[port]
				ports[port] = true
				mu.Unlock()
				if dup || held[port] || canBind(port) || canBind(port+1) {
					errs <- fmt.Errorf("%s: answered %d, duplicate=%v held=%v", p.callID, port, dup, held[port])
				}
				p.request("ACK", "")
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// 200 media starts on a loaded runner outlast waitFor's two seconds.
	for deadline := time.Now().Add(30 * time.Second); started.Load() != calls; {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d calls started", started.Load(), calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if failed.Load() != 0 {
		t.Errorf("%d calls failed after their ACK", failed.Load())
	}
	for _, rec := range logs.records(t) {
		if rec.msg() == "media failed to start" {
			t.Fatalf("media failed to start: %s", rec.raw)
		}
	}
	if len(ports) != calls {
		t.Errorf("%d distinct ports for %d calls", len(ports), calls)
	}
}

func TestUsedPortsStayConsistentWithoutRTCP(t *testing.T) {
	base := freePairBase(t, 1)
	uas := NewUAS(Config{RTPPortRange: [2]int{base, base + 1},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	pair, err := uas.allocateRTPPort(loopback, uas.log)
	if err != nil {
		t.Fatal(err)
	}
	if pair.rtcp != nil {
		t.Error("an RTCP socket was bound with RTCP off")
	}
	pair.close()
	uas.releaseRTPPort(pair.port)
	if usedPortCount(uas) != 0 {
		t.Error("ports still claimed")
	}
}
