// SPDX-License-Identifier: Apache-2.0

package loadgen_test

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/loadgen"
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// A load generator that silently measures nothing looks exactly like a system
// under no load. These tests hold it against a real UAS: the call has to be
// answered, the headers have to arrive, and audio sent back has to be counted
// and timed.

func TestPlacedCallIsAnsweredAndCarriesItsHeaders(t *testing.T) {
	uas, started := startUAS(t)

	result := loadgen.PlaceCall(t.Context(), loadgen.CallConfig{
		Target:   "127.0.0.1:" + strconv.Itoa(uas.LocalPort()),
		DID:      "95001",
		Language: "en",
		Duration: 300 * time.Millisecond,
	})
	if result.Err != nil {
		t.Fatalf("place call: %v", result.Err)
	}
	if result.FramesSent < 5 {
		t.Errorf("sent %d frames in 300ms, expected about 15", result.FramesSent)
	}

	select {
	case dialog := <-started:
		for header, want := range map[string]string{
			"X-Aicc-Did": "95001", "X-Aicc-Language": "en",
		} {
			if got := dialog.CustomHeaders[header]; got != want {
				t.Errorf("%s = %q, want %q (headers: %v)", header, got, want, dialog.CustomHeaders)
			}
		}
		if dialog.CustomHeaders["X-Aicc-Call-Id"] == "" {
			t.Error("the call arrived without an identifier")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the UAS never reported the call started")
	}
}

func TestDownlinkAudioIsCountedAndTimed(t *testing.T) {
	uas, started := startUAS(t)

	// Answer the caller with a steady stream, the way a session would.
	go func() {
		dialog := <-started
		frame := media.SilenceFrame(media.LawMu)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range 40 {
			<-ticker.C
			dialog.RTP.Send(frame)
		}
	}()

	result := loadgen.PlaceCall(t.Context(), loadgen.CallConfig{
		Target:   "127.0.0.1:" + strconv.Itoa(uas.LocalPort()),
		Duration: 700 * time.Millisecond,
	})
	if result.Err != nil {
		t.Fatalf("place call: %v", result.Err)
	}
	if result.FramesReceived < 10 {
		t.Fatalf("received %d frames, expected about 35", result.FramesReceived)
	}
	if result.FirstAudioMs < 0 {
		t.Error("audio arrived but first-audio was never timed")
	}
	// A local loopback with nothing else running has no reason to stall.
	if result.LateFrames > result.FramesReceived/4 {
		t.Errorf("%d of %d frames late on loopback (max gap %.0fms)",
			result.LateFrames, result.FramesReceived, result.MaxGapMs)
	}
}

func TestRunReportsEveryCallItPlaced(t *testing.T) {
	uas, started := startUAS(t)
	go func() {
		for range started {
		}
	}()

	report := loadgen.Run(t.Context(), loadgen.RunConfig{
		Call: loadgen.CallConfig{
			Target:   "127.0.0.1:" + strconv.Itoa(uas.LocalPort()),
			Duration: 200 * time.Millisecond,
		},
		Calls: 5,
		Ramp:  50 * time.Millisecond,
	})
	if report.Answered != 5 || report.Failed != 0 {
		t.Fatalf("answered %d failed %d of 5 placed: %v",
			report.Answered, report.Failed, report.Failures)
	}
	if report.LiveCalls != 0 {
		t.Errorf("%d calls still live after the run returned", report.LiveCalls)
	}
	if report.SetupP50Ms <= 0 {
		t.Error("setup was never timed")
	}
}

func TestUnroutableTargetIsReportedAsAFailure(t *testing.T) {
	// A port nothing is listening on: the INVITE goes nowhere and the run has
	// to say so rather than report a clean zero-call success.
	report := loadgen.Run(t.Context(), loadgen.RunConfig{
		Call: loadgen.CallConfig{
			Target:   "127.0.0.1:" + strconv.Itoa(freePort(t)),
			Duration: 100 * time.Millisecond,
		},
		Calls: 1,
	})
	if report.Failed != 1 || report.Answered != 0 {
		t.Fatalf("answered %d failed %d, want 0 and 1", report.Answered, report.Failed)
	}
	if len(report.Failures) != 1 {
		t.Errorf("failures = %v, want one category", report.Failures)
	}
}

func startUAS(t *testing.T) (*voice.UAS, chan *voice.Dialog) {
	t.Helper()

	cfg := voice.DefaultConfig()
	cfg.SIPHost = "127.0.0.1"
	cfg.SIPPort = 0
	cfg.AdvertiseIP = "127.0.0.1"
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	uas := voice.NewUAS(cfg)
	started := make(chan *voice.Dialog, 8)
	uas.OnCallStarted = func(d *voice.Dialog) { started <- d }
	if err := uas.Start(); err != nil {
		t.Fatalf("start uas: %v", err)
	}
	t.Cleanup(uas.Stop)
	return uas, started
}

func freePort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port
}
