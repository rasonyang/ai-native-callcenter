// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
)

// Fault names the way the relay misbehaves in front of a real provider. Each
// reproduces a shape seen live on the qwen endpoint (#197, #310), so a fix in
// the Realtime client can be exercised on real calls without waiting for the
// provider to fail on its own.
type Fault string

const (
	// FaultNone relays every frame untouched.
	FaultNone Fault = ""
	// FaultStall drops the audio of one response and holds its response.done;
	// the provider honours the client's response.cancel (#310, 2026-10-22).
	FaultStall Fault = "stall"
	// FaultStallHold is FaultStall where the provider ignores the cancel and
	// refuses a new response while the stalled one is in progress (#310,
	// 2026-11-07).
	FaultStallHold Fault = "stall-hold"
	// FaultUnanswered makes the provider never answer one caller turn: the
	// response is cancelled upstream and hidden from the client (#310).
	FaultUnanswered Fault = "unanswered"
	// FaultVADRefused is FaultUnanswered plus the error qwen sends when a
	// server VAD turn is committed but no response can be created (#197).
	FaultVADRefused Fault = "vad-refused"
)

// ParseFault validates a -fault value.
func ParseFault(s string) (Fault, error) {
	switch f := Fault(s); f {
	case FaultNone, FaultStall, FaultStallHold, FaultUnanswered, FaultVADRefused:
		return f, nil
	}
	return "", fmt.Errorf("unknown fault %q (want stall, stall-hold, unanswered or vad-refused)", s)
}

// TurnRange is an inclusive range of turn numbers.
type TurnRange struct{ Lo, Hi int }

// ParseTurnRange reads "2-4" or a single "3".
func ParseTurnRange(s string) (TurnRange, error) {
	lo, hi, found := strings.Cut(s, "-")
	if !found {
		hi = lo
	}
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	h, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || l < 1 || h < l {
		return TurnRange{}, fmt.Errorf("fault turns %q: want N or LO-HI with 1 <= LO <= HI", s)
	}
	return TurnRange{Lo: l, Hi: h}, nil
}

// decide settles whether a session is faulted and on which of its eligible
// turns (the caller turns the provider's VAD answered). It is a pure function of the seed and the session number, so a run
// can be repeated and a session's fate does not depend on how many sessions
// raced it or in which order they arrived.
func decide(seed int64, session int, ratio float64, turns TurnRange) (faulted bool, turn int) {
	rng := rand.New(rand.NewPCG(uint64(seed), uint64(session)))
	faulted = rng.Float64() < ratio
	turn = turns.Lo + rng.IntN(turns.Hi-turns.Lo+1)
	return faulted, turn
}
