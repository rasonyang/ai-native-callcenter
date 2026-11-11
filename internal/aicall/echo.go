// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/echoprobe"
	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// The echo measurement (internal/echoprobe) watches two signals and alters
// neither. The echo suppressor (AICC_BOT_ECHO_SUPPRESS, off by default) runs
// inside it and alters only what the provider is fed, never what the
// measurement, its verdicts or the dump see.
//
//   - The reference is what the leg put on the wire. It is tapped where the
//     RTP session's send loop hands a frame to the socket (voice.RTPSession.Run,
//     via SetTxTap), not where the session queues it, so a barge-in flush of
//     the TX queue shows in it as the silence the caller really got. That loop
//     runs on its own goroutine: the tap copies the frame into a short queue
//     and returns.
//   - The uplink is the frame uplinkClock emits for each 20 ms slot, real or
//     fill, observed in sendDueSlots on the uplink pump's goroutine, the one
//     that owns the clock. It is the same time axis: the monitor starts at the
//     clock's start and places both signals on it.
//   - Suppression happens after that observation, on the same goroutine and
//     the same slot: a real frame (never a fill, never a hushed slot) that the
//     suppressor judged to hold only the bot's echo is overwritten with the
//     law's silence before feedSlot converts it, so the provider still gets
//     one frame per slot. On the G.711 passthrough path that means the
//     suppressor re-encodes the slots where its gate opens or closes; the
//     bytes it leaves alone are the bytes that came in.

// startEchoProbe creates the call's measurement, attaches it to the leg's send
// loop and publishes it for the barge-in path. It returns nil, measuring
// nothing, when the leg cannot report what it sends.
func (s *Session) startEchoProbe(start time.Time) *echoprobe.Monitor {
	tapper, ok := s.leg.(txTapper)
	if !ok {
		return nil
	}
	cfg := echoprobe.Config{Start: start, Law: s.leg.Law(), Logger: s.log,
		Suppress: s.cfg.IsEchoSuppressionEnabled}
	if dir := s.cfg.EchoDumpDir; dir != "" && s.ledgerCallID != uuid.Nil {
		path := filepath.Join(dir, s.ledgerCallID.String()+".wav")
		cfg.Dump = echoprobe.NewDumper(func() (io.WriteSeeker, io.Closer, error) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, nil, err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return nil, nil, err
			}
			s.log.Info("echo dump started", "path", path)
			return f, f, nil
		}, echoprobe.LogErrorOnce(s.log))
	}
	probe := echoprobe.New(cfg)
	tapper.SetTxTap(probe.PushReference)
	s.echo.Store(probe)
	return probe
}

// finishEchoProbe detaches the measurement, logs what it found and publishes
// the call's metrics.
func (s *Session) finishEchoProbe() {
	probe := s.echo.Load()
	if probe == nil {
		return
	}
	if tapper, ok := s.leg.(txTapper); ok {
		tapper.SetTxTap(nil)
	}
	sum := probe.Summary()
	result := obs.EchoResultUnmeasured
	switch {
	case sum.IsMeasured && sum.IsEchoFound:
		result = obs.EchoResultFound
	case sum.IsMeasured:
		result = obs.EchoResultNotFound
	}
	obs.RecordEchoCall(result, int64(sum.DelayMs), sum.ReturnLossDb)
	// The trunk is not known to the bot leg (its INVITE comes from the
	// aicc_bot gateway, not the carrier's); callId joins this line to the CDR.
	s.log.Info("ai echo measurement",
		"result", result,
		"isEchoFound", sum.IsEchoFound,
		"echoDelayMs", sum.DelayMs,
		"echoReturnLossDb", sum.ReturnLossDb,
		"echoReturnLossMinDb", sum.ReturnLossMinDb,
		"echoReturnLossMaxDb", sum.ReturnLossMaxDb,
		"isCandidateOnly", sum.IsCandidateOnly,
		"estimates", sum.Estimates,
		"estimatesFoundEcho", sum.EstimatesFoundEcho,
		"finalRunEstimates", sum.FinalRunEstimates,
		"botSpeechMs", sum.BotSpeechMs,
		"referenceFramesDropped", sum.ReferenceDropped,
		"dumpFramesDropped", sum.DumpDropped,
		"isMeasurementFailed", sum.IsFailed)

	sup := sum.Suppression
	state := obs.EchoSuppressionOff
	switch {
	case !sup.IsOn:
	case sup.IsFailed:
		state = obs.EchoSuppressionFailed
	case sup.FramesSuppressed > 0:
		state = obs.EchoSuppressionEngaged
	default:
		state = obs.EchoSuppressionIdle
	}
	obs.RecordEchoSuppression(state, sup.FramesSuppressed, sup.GateOpenings)
	s.log.Info("ai echo suppression",
		"isEchoSuppressionOn", sup.IsOn,
		"state", state,
		"framesExamined", sup.FramesExamined,
		"framesSuppressed", sup.FramesSuppressed,
		"gateOpenings", sup.GateOpenings,
		"framesCorrelated", sup.FramesCorrelated,
		"framesWithEchoDelay", sup.FramesWithDelay,
		"lagWindowFromMs", sup.LagFromMs,
		"lagWindowToMs", sup.LagToMs,
		"isLagWindowFromEchoDelay", sup.IsLagFromDelay,
		"echoGainDb", sup.EchoGainDb,
		"onsetEchoGainDb", sup.OnsetEchoGainDb,
		"isSuppressionFailed", sup.IsFailed)
}

// withEchoVerdict judges a speech interruption for the echo measurement: it
// asks whether the last 600 ms of uplink are the bot's own voice coming back,
// counts the verdict, and adds isEchoLikely to the interruption's log
// attributes. Measurement only: it changes nothing about the interruption,
// and both callers run it after the flush and after the provider is told (the
// waveform correlation is not on a per-frame path). An unknown verdict
// (nothing to judge from, or a weak correlation with no echo delay known) is
// not "not echo" and adds no isEchoLikely; an unmeasured call (a leg that
// cannot report what it sends) is neither counted nor annotated.
func (s *Session) withEchoVerdict(args []any) []any {
	probe := s.echo.Load()
	if probe == nil {
		return args
	}
	j := probe.Judge()
	obs.RecordSpeechInterruption(string(j.Verdict))
	switch j.Verdict {
	case echoprobe.VerdictEcho:
		args = append(args, "isEchoLikely", true)
	case echoprobe.VerdictNotEcho:
		args = append(args, "isEchoLikely", false)
	}
	if j.IsCorrelated {
		// The peak the verdict was taken from, whatever it was, so that its
		// thresholds can be re-derived from logs.
		args = append(args,
			"echoCorrelation", j.Correlation,
			"echoSecondCorrelation", j.SecondCorrelation,
			"echoLagMs", j.LagMs,
			"echoUplinkLevelDb", j.UplinkLevelDb)
	}
	return args
}
