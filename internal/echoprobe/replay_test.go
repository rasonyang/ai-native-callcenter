// SPDX-License-Identifier: Apache-2.0

package echoprobe

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// TestReplayADump feeds a dump written by AICC_BOT_ECHO_DUMP_DIR back through
// a Monitor, slot by slot on the dump's own time axis, and prints each estimate
// and the judgement at the given offsets (seconds from the dump's start), as
// an interruption there would have been judged. It is a development aid for
// live rounds and skips unless a dump is named (the dumps hold call audio and
// are never committed):
//
//	AICC_ECHO_REPLAY_WAV=/path/<callId>.wav AICC_ECHO_REPLAY_AT=1.6,3.7 \
//	  go test -run TestReplayADump -v ./internal/echoprobe/
func TestReplayADump(t *testing.T) {
	path := os.Getenv("AICC_ECHO_REPLAY_WAV")
	if path == "" {
		t.Skip("AICC_ECHO_REPLAY_WAV names no dump")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < wavHeaderSize {
		t.Fatalf("%s is not a dump", path)
	}
	pcm := data[wavHeaderSize:]
	slots := len(pcm) / (frameSamples * dumpChannels * 2)

	type mark struct {
		slot   int
		window map[Verdict]int // every other slot within ±200 ms, by verdict
		at     Judgement
	}
	var marks []*mark
	if at := os.Getenv("AICC_ECHO_REPLAY_AT"); at != "" {
		for _, f := range strings.Split(at, ",") {
			sec, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
			if err != nil {
				t.Fatal(err)
			}
			marks = append(marks, &mark{slot: int(sec*50 + 0.5), window: map[Verdict]int{}})
		}
	}

	start := time.Unix(1_000_000, 0)
	m := New(Config{Start: start, Law: media.LawMu})
	var left, right [frameSamples]int16
	refEnc := make([]byte, 0, frameSamples)
	upEnc := make([]byte, 0, frameSamples)
	estimates, candidateAt := 0, -1
	for k := 0; k < slots; k++ {
		for i := range frameSamples {
			at := ((k*frameSamples + i) * dumpChannels) * 2
			left[i] = int16(binary.LittleEndian.Uint16(pcm[at:]))
			right[i] = int16(binary.LittleEndian.Uint16(pcm[at+2:]))
		}
		refEnc = m.law.Encode(refEnc, left[:])
		m.PushReference(refEnc, start.Add(time.Duration(k)*20*time.Millisecond))
		upEnc = m.law.Encode(upEnc, right[:])
		m.FeedUplink(upEnc)

		if m.track.estimates != estimates {
			estimates = m.track.estimates
			e := m.track.lastSeen
			t.Logf("%6.2fs estimate %d: found=%v lag=%dms corr=%.2f second=%.2f pairs=%d loss=%.1fdB published=%v/%dms",
				float64(k)/50, estimates, e.isFound, e.lagBins*binMs, e.corr, e.second, e.pairs,
				e.returnLossDb(), m.track.isFound, m.track.lagBins*binMs)
			if e.isFound && candidateAt < 0 {
				candidateAt = k
			}
		}
		for _, mk := range marks {
			if k < mk.slot-10 || k > mk.slot+10 || (k-mk.slot)%2 != 0 {
				continue
			}
			// Judging records what a strong verdict measures; the window
			// around a mark is judged on a copy, so only the mark itself
			// leaves its trace, as the one live interruption did.
			j := judgeAside(m)
			if k == mk.slot {
				j = m.Judge()
				mk.at = j
			}
			mk.window[j.Verdict]++
			if os.Getenv("AICC_ECHO_REPLAY_VERBOSE") != "" {
				t.Logf("    %6.2fs %-8s corr=%.2f lag=%dms second=%.2f level=%.1fdB",
					float64(k)/50, j.Verdict, j.Correlation, j.LagMs, j.SecondCorrelation, j.UplinkLevelDb)
			}
		}
	}
	if candidateAt >= 0 {
		t.Logf("first echo path known at %.2fs", float64(candidateAt)/50)
	}
	for _, mk := range marks {
		j := mk.at
		t.Logf("%6.2fs %-8s corr=%.2f lag=%dms second=%.2f level=%.1fdB (±200 ms every 40 ms: %d ECHO, %d NOT_ECHO, %d UNKNOWN)",
			float64(mk.slot)/50, j.Verdict, j.Correlation, j.LagMs, j.SecondCorrelation, j.UplinkLevelDb,
			mk.window[VerdictEcho], mk.window[VerdictNotEcho], mk.window[VerdictUnknown])
	}
	t.Logf("summary %s", fmt.Sprintf("%+v", m.Summary()))
}

// judgeAside judges as Judge does and then puts back what a strong verdict
// records, so that looking around a moment does not change what the moment
// itself is judged with.
func judgeAside(m *Monitor) Judgement {
	m.mu.Lock()
	lag, has, loudest := m.waveLagMs, m.hasWaveLag, m.waveLoudestGain
	m.mu.Unlock()
	j := m.Judge()
	m.mu.Lock()
	m.waveLagMs, m.hasWaveLag, m.waveLoudestGain = lag, has, loudest
	m.mu.Unlock()
	return j
}
