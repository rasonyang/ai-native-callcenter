// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// suppressVariants are rule sets the offline comparison runs beside the
// product's (defaultSuppressParams, "default"), each named for how it differs
// from it. Test-only: the product has one.
var suppressVariants = map[string]func(p *suppressParams){
	"default": func(*suppressParams) {},
	// The study's B-bal rules: learns only from slots that correlate at 0.6.
	"bal": func(p *suppressParams) { p.learnCorr = 0.6 },
	// The study's B-fast: B-bal's correlation, learning from one slot, with
	// no rate limit and no ceiling.
	"fast": func(p *suppressParams) {
		p.learnCorr, p.stableSlots, p.attackDb, p.ceilingDb = 0.6, 1, math.Inf(1), math.Inf(1)
	},
	// Other ways of learning sooner, each from B-bal's rules: no rate limit;
	// one slot is enough; a looser lag agreement.
	"bal-noattack": func(p *suppressParams) { p.learnCorr, p.attackDb = 0.6, math.Inf(1) },
	"bal-stable1":  func(p *suppressParams) { p.learnCorr, p.stableSlots = 0.6, 1 },
	"bal-lag5ms":   func(p *suppressParams) { p.learnCorr, p.stableSamples = 0.6, 40 },
	// What the other guards hold with the stable-lag guard removed (as if a
	// chance pair of slots got through it).
	"stable1":          func(p *suppressParams) { p.stableSlots = 1 },
	"bal-noattack-st1": func(p *suppressParams) { p.learnCorr, p.attackDb, p.stableSlots = 0.6, math.Inf(1), 1 },
	// The window before a delay is known: the study's, and the widest the
	// measurement searches.
	"wide200-700":  func(p *suppressParams) { p.wideFromMs = 200 },
	"wide20-1000":  func(p *suppressParams) { p.wideToMs = sxMaxLagMs },
	"wide200-1000": func(p *suppressParams) { p.wideFromMs, p.wideToMs = 200, sxMaxLagMs },
	"bal-wide20-1000": func(p *suppressParams) {
		p.learnCorr, p.wideToMs = 0.6, sxMaxLagMs
	},
	"bal-wide200-700": func(p *suppressParams) { p.learnCorr, p.wideFromMs = 0.6, 200 },
	// Starting transparent: no echo assumed until one has been learned.
	"transparent-start": func(p *suppressParams) { p.initDb, p.onsetInitDb = p.floorDb, p.floorDb },
}

// TestSuppressADump runs the suppressor over dumps written by
// AICC_BOT_ECHO_DUMP_DIR exactly as a call runs it: slot by slot on the dump's
// own time axis, the reference pushed as the leg's send loop pushes it, the
// measurement fed the raw uplink, and the suppressor's delay taken from that
// measurement as it learns it. It writes what the provider would have been
// fed, decoded, as a mono 8 kHz WAV per dump (and a summary beside it) into
// AICC_ECHO_SUPPRESS_OUT, for scoring offline. A development aid that skips
// unless dumps are named; the dumps and its output hold call audio and are
// never committed:
//
//	AICC_ECHO_SUPPRESS_WAVS=/path/dumps AICC_ECHO_SUPPRESS_OUT=/path/out \
//	  AICC_ECHO_SUPPRESS_VARIANT=default go test -run TestSuppressADump -v ./internal/echoprobe/
//
// AICC_ECHO_SUPPRESS_WAVS is a directory of dumps or a comma-separated list.
func TestSuppressADump(t *testing.T) {
	in, out := os.Getenv("AICC_ECHO_SUPPRESS_WAVS"), os.Getenv("AICC_ECHO_SUPPRESS_OUT")
	if in == "" || out == "" {
		t.Skip("AICC_ECHO_SUPPRESS_WAVS or AICC_ECHO_SUPPRESS_OUT is not set")
	}
	if !filepath.IsAbs(out) {
		t.Fatalf("AICC_ECHO_SUPPRESS_OUT must be absolute, got %q", out)
	}
	variant := os.Getenv("AICC_ECHO_SUPPRESS_VARIANT")
	if variant == "" {
		variant = "default"
	}
	adjust, ok := suppressVariants[variant]
	if !ok {
		t.Fatalf("unknown variant %q", variant)
	}
	var paths []string
	if st, err := os.Stat(in); err == nil && st.IsDir() {
		paths, _ = filepath.Glob(filepath.Join(in, "*.wav"))
	} else {
		paths = strings.Split(in, ",")
	}
	sort.Strings(paths)
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".wav")
		sum, trace, processed := suppressDump(t, path, adjust)
		writeMonoWAV(t, filepath.Join(out, name+".wav"), processed)
		report := map[string]any{"variant": variant, "summary": sum, "firstDelaySlot": trace.firstDelaySlot,
			"elapsedNsPerSlot": trace.nsPerSlot, "slots": len(processed) / frameSamples}
		data, _ := json.MarshalIndent(report, "", " ")
		if err := os.WriteFile(filepath.Join(out, name+".json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name+".gates.csv"), []byte(trace.csv.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		s := sum.Suppression
		t.Logf("%s %s: examined %d suppressed %d openings %d correlated %d withDelay %d window %d-%d ms (delay %v) gain %.1f onset %.1f; delay known from slot %d; %.0f us/slot",
			name[:13], variant, s.FramesExamined, s.FramesSuppressed, s.GateOpenings, s.FramesCorrelated, s.FramesWithDelay,
			s.LagFromMs, s.LagToMs, s.IsLagFromDelay, s.EchoGainDb, s.OnsetEchoGainDb, trace.firstDelaySlot, trace.nsPerSlot/1000)
	}
}

type dumpTrace struct {
	firstDelaySlot int
	nsPerSlot      float64
	csv            strings.Builder
}

// suppressDump replays one dump and returns the summary, a per-slot trace
// and the processed uplink as PCM.
func suppressDump(t *testing.T, path string, adjust func(*suppressParams)) (Summary, *dumpTrace, []int16) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < wavHeaderSize {
		t.Fatalf("%s is not a dump", path)
	}
	pcm := data[wavHeaderSize:]
	slots := len(pcm) / (frameSamples * dumpChannels * 2)

	start := time.Unix(1_000_000, 0)
	m := New(Config{Start: start, Law: media.LawMu, Suppress: true})
	adjust(&m.sup.p)
	m.sup.est, m.sup.onest = m.sup.p.initDb, m.sup.p.onsetInitDb
	m.sup.fromMs, m.sup.toMs = m.sup.p.wideFromMs, m.sup.p.wideToMs

	tr := &dumpTrace{firstDelaySlot: -1}
	tr.csv.WriteString("slot,isOpen,est,onest,fromMs,toMs,isHinted\n")
	var left, right [frameSamples]int16
	refEnc := make([]byte, 0, frameSamples)
	upEnc := make([]byte, 0, frameSamples)
	outPCM := make([]int16, 0, slots*frameSamples)
	var dec [frameSamples]int16
	var elapsed time.Duration
	for k := 0; k < slots; k++ {
		for i := range frameSamples {
			at := ((k*frameSamples + i) * dumpChannels) * 2
			left[i] = int16(binary.LittleEndian.Uint16(pcm[at:]))
			right[i] = int16(binary.LittleEndian.Uint16(pcm[at+2:]))
		}
		refEnc = m.law.Encode(refEnc, left[:])
		m.PushReference(refEnc, start.Add(time.Duration(k)*20*time.Millisecond))
		upEnc = m.law.Encode(upEnc, right[:])
		began := time.Now()
		m.FeedUplink(upEnc)
		m.Suppress(upEnc)
		elapsed += time.Since(began)
		outPCM = append(outPCM, m.law.Decode(dec[:0], upEnc)...)

		s := m.sup
		if s.isHinted && tr.firstDelaySlot < 0 {
			tr.firstDelaySlot = k
		}
		open := 0
		if s.isOpen {
			open = 1
		}
		fmt.Fprintf(&tr.csv, "%d,%d,%.2f,%.2f,%d,%d,%v\n", k, open, s.est, s.onest, s.fromMs, s.toMs, s.isHinted)
	}
	tr.nsPerSlot = float64(elapsed.Nanoseconds()) / float64(max(slots, 1))
	return m.Summary(), tr, outPCM
}

// writeMonoWAV writes 8 kHz 16-bit mono PCM.
func writeMonoWAV(t *testing.T, path string, pcm []int16) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	size := uint32(len(pcm) * 2)
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], 36+size)
	copy(hdr[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], 8000)
	binary.LittleEndian.PutUint32(hdr[28:], 16000)
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], size)
	w.Write(hdr)
	var b [2]byte
	for _, v := range pcm {
		binary.LittleEndian.PutUint16(b[:], uint16(v))
		w.Write(b[:])
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
}
