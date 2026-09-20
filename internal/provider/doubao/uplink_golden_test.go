// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the uplink looks like, byte for byte.
//
// The other uplink tests decode a frame and ask whether the field they care
// about is right. That leaves the cadence itself — which frame, in which order,
// on which tick — described only by the assertions somebody thought to write.
// This one records every frame a scripted call put on the wire, in order and as
// bytes, and compares the lot to a file. On this protocol the order IS the
// promise: an append before the resume that announced it, or a burst where one
// frame was due, is what the engine calls a pacing error.
//
// Run with -update to rewrite the golden, and read the diff before committing
// it: a changed golden is a changed cadence.
var isUplinkGoldenUpdate = flag.Bool("update", false,
	"rewrite the golden uplink frames under testdata/")

// uplinkFrame is a frame of caller audio short enough to read in the golden.
// The bytes are not audio and need not be: what is pinned here is the frame
// around them.
func uplinkFrame(marker byte) []byte {
	return []byte{marker, 0x7f, 0x80, 0xff}
}

// A held call, frame by frame: a jitter burst that has to lose its oldest, the
// hold declared once when the caller's side goes quiet, and the resume that
// goes ahead of the first frame after it.
func TestTheUplinkFramesOfAHeldCallAreWhatTheyWere(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session, tick := pacedSession(t, f)

	// Audio arrives from the telephone leg faster than the cadence takes it.
	// The queue holds three; the two oldest are the ones the caller has moved
	// past, and they never reach the wire.
	for _, marker := range []byte{0x01, 0x02, 0x03, 0x04, 0x05} {
		if err := session.SendAudio(uplinkFrame(marker)); err != nil {
			t.Fatalf("send audio: %v", err)
		}
	}
	for range 3 {
		tick()
	}

	// Silence on the leg. The engine reads the uplink as a keepalive, so the
	// hold is declared — once, not on every tick of it.
	for range muteAfterEmptyTicks {
		tick()
	}

	// They speak again. The resume is a frame the engine has to be ready for,
	// so it goes first and on the same tick as the audio it precedes.
	if err := session.SendAudio(uplinkFrame(0x06)); err != nil {
		t.Fatalf("send audio after the hold: %v", err)
	}
	tick()

	checkUplinkGolden(t, "uplink_held_call", f.settledFrames(7))

	stats := session.Stats()
	if stats.FramesSent != 4 || stats.FramesDropped != 2 ||
		stats.Mutes != 1 || stats.Unmutes != 1 {
		t.Errorf("the client counted %+v, want four sent, two dropped, one hold and one resume",
			stats)
	}
}

func checkUplinkGolden(t *testing.T, name string, frames [][]byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".jsonl")

	if *isUplinkGoldenUpdate {
		var file bytes.Buffer
		for _, frame := range frames {
			if bytes.ContainsRune(frame, '\n') {
				t.Fatalf("frame %s spans lines, which this format cannot hold", frame)
			}
			file.Write(frame)
			file.WriteByte('\n')
		}
		if err := os.WriteFile(path, file.Bytes(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("wrote %d frames to %s", len(frames), path)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden frames (run with -update to write them): %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(frames) != len(want) {
		t.Fatalf("the client sent %d frames, want %d:\n got: %s\nwant: %s",
			len(frames), len(want),
			strings.Join(frameStrings(frames), "\n      "),
			strings.Join(want, "\n      "))
	}
	for i := range frames {
		if string(frames[i]) != want[i] {
			t.Errorf("frame %d is not what the provider used to see:\n got: %s\nwant: %s",
				i, frames[i], want[i])
		}
	}
}

func frameStrings(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, string(frame))
	}
	return out
}
