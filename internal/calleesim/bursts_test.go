// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"context"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

func TestBurstSourceSoundsOnlyInsideItsBursts(t *testing.T) {
	src := newBurstSource(media.LawMu, 300, []Burst{{AtMs: 100, DurationMs: 60}, {AtMs: 500, DurationMs: 20}})
	for _, c := range []struct {
		at   time.Duration
		want bool
	}{
		{0, false}, {99 * time.Millisecond, false},
		{100 * time.Millisecond, true}, {159 * time.Millisecond, true}, {160 * time.Millisecond, false},
		{499 * time.Millisecond, false}, {500 * time.Millisecond, true}, {520 * time.Millisecond, false},
		{time.Hour, false},
	} {
		frame := src.frameAt(c.at)
		if got := frame != nil; got != c.want {
			t.Errorf("at %v: a frame was sent = %v, want %v", c.at, got, c.want)
			continue
		}
		if frame != nil && frameRMS(media.LawMu, frame) <= silenceRMS {
			t.Errorf("at %v: the burst's frame is silent", c.at)
		}
	}
}

func TestBurstsScenarioIsValidated(t *testing.T) {
	good := Behaviour{Uplink: Uplink{Kind: UplinkBursts, Bursts: []Burst{{AtMs: 0, DurationMs: 20}}}}
	if err := good.validate(); err != nil {
		t.Fatalf("a valid BURSTS uplink was refused: %v", err)
	}
	for name, up := range map[string]Uplink{
		"no bursts":        {Kind: UplinkBursts},
		"negative at":      {Kind: UplinkBursts, Bursts: []Burst{{AtMs: -1, DurationMs: 20}}},
		"zero duration":    {Kind: UplinkBursts, Bursts: []Burst{{AtMs: 0}}},
		"bursts on a tone": {Kind: UplinkTone, Bursts: []Burst{{AtMs: 0, DurationMs: 20}}},
	} {
		b := Behaviour{Uplink: up}
		if err := b.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAudioRunsSplitOnAPauseAndIgnoreJitter(t *testing.T) {
	var d Downlink
	for _, at := range []float64{1000, 1020, 1040, 1130, 1150} { // 90 ms gap: one run
		d.addAudio(at)
	}
	for _, at := range []float64{1400, 1420} { // 250 ms gap: a new run
		d.addAudio(at)
	}
	want := []AudioRun{
		{StartAfterInviteMs: 1000, EndAfterInviteMs: 1150, Frames: 5},
		{StartAfterInviteMs: 1400, EndAfterInviteMs: 1420, Frames: 2},
	}
	if len(d.AudioRuns) != len(want) {
		t.Fatalf("runs = %+v, want %+v", d.AudioRuns, want)
	}
	for i := range want {
		if d.AudioRuns[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, d.AudioRuns[i], want[i])
		}
	}
}

// A callee with a BURSTS uplink sends its tone where the scenario says, timed
// from the moment media started, which the record states.
func TestBurstsUplinkIsTimedFromMediaStart(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{
		Uplink: Uplink{Kind: UplinkBursts, Bursts: []Burst{{AtMs: 300, DurationMs: 200}}},
	}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	uac.waitFinal()
	uac.ack()

	// The burst has been heard and ended once frames keep arriving 700 ms
	// after the first one (silence fills the gaps by itself).
	eventually(t, "the burst to have been heard and to have ended", func() bool {
		frames := uac.uplinkFrames()
		if len(frames) == 0 || frames[len(frames)-1].at.Sub(frames[0].at) < 700*time.Millisecond {
			return false
		}
		for _, f := range frames {
			if f.rms > silenceRMS {
				return true
			}
		}
		return false
	})
	rec := only(t, sim.Report())
	if rec.UplinkStartedAfterInviteMs == nil {
		t.Fatal("the record does not say when the uplink started")
	}
	frames := uac.uplinkFrames()
	var first, last time.Time
	var loud int
	for _, f := range frames {
		if f.rms > silenceRMS {
			if first.IsZero() {
				first = f.at
			}
			last = f.at
			loud++
		}
	}
	// 200 ms of tone is ten frames; the bound is loose on purpose, a loaded
	// machine can drop a few, and a floor on the shape is what is asserted.
	if loud < 6 || loud > 12 {
		t.Errorf("loud frames = %d, want about 10", loud)
	}
	if span := last.Sub(first); span > 300*time.Millisecond {
		t.Errorf("the burst lasted %v, want about 200 ms", span)
	}
	// Silence before the burst: the first frames of the session are not loud.
	if len(frames) > 0 && frames[0].rms > silenceRMS {
		t.Error("the first frame is loud, the burst was to start 300 ms in")
	}
}

func TestDownlinkAudioRunsAreRecorded(t *testing.T) {
	sim := startSim(t, loopback, scenarioOf(t, map[string][]Behaviour{"1": {{}}}), nil)
	uac := newUAC(t, loopback, sim.Addr(), "1")
	uac.invite()
	final := uac.waitFinal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	uac.sendDownlink(ctx, remoteRTP(t, final), 300*time.Millisecond, 500*time.Millisecond)
	// A sender starved of CPU can pause for more than a run's 100 ms gap, so
	// how the audio splits into runs is not asserted here (the unit test above
	// owns that); the runs together must cover the audio, after the silence.
	var runs []AudioRun
	frames := func() (n int) {
		for _, r := range runs {
			n += r.Frames
		}
		return n
	}
	eventually(t, "the audio runs", func() bool {
		runs = only(t, sim.Report()).Downlink.AudioRuns
		return frames() >= 20
	})
	first, last := runs[0], runs[len(runs)-1]
	if first.StartAfterInviteMs < 250 || last.EndAfterInviteMs-first.StartAfterInviteMs < 350 {
		t.Errorf("runs = %+v, want them to start after the 300 ms of silence and span about 500 ms", runs)
	}
}
