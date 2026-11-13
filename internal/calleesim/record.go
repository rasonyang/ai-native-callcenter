// SPDX-License-Identifier: Apache-2.0

package calleesim

import (
	"encoding/json"
	"os"
	"time"
)

// Who ended a dialog, and with which request.
const (
	EndedByCaller = "CALLER"
	EndedByCallee = "CALLEE"

	EndedWithBye    = "BYE"
	EndedWithCancel = "CANCEL"
)

// energyWindowMs is the width of one downlink energy bucket.
const energyWindowMs = 100

// Report is everything the simulator saw, in the order the dialogs began.
//
// Every event is recorded on two clocks. The xxxAt stamps are the wall clock,
// which can step (on Colima the VM's is set from the host every 10 s, issues
// #506 and #515). The xxxMs values beside them are the simulator's monotonic
// clock, which does not: afterInviteMs is a time inside one dialog, and
// afterStartMs places an event on the one timeline every dialog of this
// process shares, counted from the simulator's start. A duration or an order
// is read from the monotonic values; the gap between a monotonic value and the
// matching difference of wall stamps is how a step of the wall clock shows up.
type Report struct {
	// Failed is set when any dialog was unexpected.
	Failed          bool `json:"failed"`
	UnexpectedCount int  `json:"unexpectedCount"`
	// StartedAt is when the simulator started, on the wall clock: the zero of
	// every afterStartMs.
	StartedAt time.Time `json:"startedAt"`
	// AfterStartMs is when this report was taken, on the simulator's
	// monotonic clock: every INVITE that arrived before that moment is in it.
	AfterStartMs float64        `json:"afterStartMs"`
	Dialogs      []DialogRecord `json:"dialogs"`
}

// WriteFile saves the report as indented JSON.
func (r Report) WriteFile(path string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// DialogRecord is one INVITE's history. Timestamps are UTC wall-clock stamps;
// the monotonic values beside them are described on Report.
type DialogRecord struct {
	CallID string `json:"callId"`
	// Number is the user part of the Request-URI; Attempt counts the dialogs
	// that arrived for it before this one, from zero.
	Number  string `json:"number"`
	Attempt int    `json:"attempt"`
	// IsExpected is false for a number the scenario does not know.
	IsExpected bool   `json:"isExpected"`
	RemoteAddr string `json:"remoteAddr"`

	InviteAt time.Time `json:"inviteAt"`
	// InviteAfterStartMs is InviteAt on the simulator's monotonic timeline:
	// how long after the simulator started the INVITE arrived.
	InviteAfterStartMs float64           `json:"inviteAfterStartMs"`
	RequestURI         string            `json:"requestUri"`
	InviteHeaders      map[string]string `json:"inviteHeaders"`

	Provisionals []ProvisionalRecord `json:"provisionals"`
	Final        *FinalRecord        `json:"final,omitempty"`
	AckAt        *time.Time          `json:"ackAt,omitempty"`
	End          *EndRecord          `json:"end,omitempty"`

	// Codec is the negotiated law (PCMU or PCMA); empty when none was.
	Codec    string   `json:"codec,omitempty"`
	Downlink Downlink `json:"downlink"`
}

// ProvisionalRecord is a 1xx the simulator sent.
type ProvisionalRecord struct {
	Status        int       `json:"status"`
	SentAt        time.Time `json:"sentAt"`
	HasEarlyMedia bool      `json:"hasEarlyMedia"`
}

// FinalRecord is the final response to the INVITE.
type FinalRecord struct {
	Status int       `json:"status"`
	SentAt time.Time `json:"sentAt"`
	// AfterInviteMs is how long after the INVITE the response was sent, and
	// AfterStartMs how long after the simulator started, both on its monotonic
	// clock. SentAt minus the dialog's InviteAt is the wall clock's reading of
	// the first; the two differ when the wall clock stepped in between.
	AfterInviteMs float64 `json:"afterInviteMs"`
	AfterStartMs  float64 `json:"afterStartMs"`
}

// EndRecord is the request that ended the dialog: a BYE from either side, or
// the caller's CANCEL.
type EndRecord struct {
	Method string    `json:"method"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	// AfterInviteMs is how long after the INVITE this request arrived, and
	// AfterStartMs how long after the simulator started, both on its monotonic
	// clock. At minus the dialog's InviteAt is the wall clock's reading of the
	// first; the two differ when the wall clock stepped in between.
	AfterInviteMs float64 `json:"afterInviteMs"`
	AfterStartMs  float64 `json:"afterStartMs"`
}

// Downlink is the audio the caller sent.
type Downlink struct {
	Frames int `json:"frames"`
	// Energy has one bucket per 100 ms window that held a frame, counted from
	// the INVITE.
	Energy []EnergyBucket `json:"energy"`
	// FirstAudioAfterAnswerMs runs from the 200 OK to the first frame above
	// silence that arrived after it; absent when there was none.
	FirstAudioAfterAnswerMs *float64 `json:"firstAudioAfterAnswerMs,omitempty"`
}

// EnergyBucket is the level of the frames received in one window.
type EnergyBucket struct {
	StartMs int     `json:"startMs"`
	Frames  int     `json:"frames"`
	MeanRMS float64 `json:"meanRms"`
	PeakRMS float64 `json:"peakRms"`

	sum float64
}

func (r *DialogRecord) snapshot() DialogRecord {
	out := *r
	out.InviteHeaders = make(map[string]string, len(r.InviteHeaders))
	for k, v := range r.InviteHeaders {
		out.InviteHeaders[k] = v
	}
	out.Provisionals = append([]ProvisionalRecord{}, r.Provisionals...)
	out.Downlink.Energy = append([]EnergyBucket{}, r.Downlink.Energy...)
	if r.Final != nil {
		f := *r.Final
		out.Final = &f
	}
	if r.AckAt != nil {
		a := *r.AckAt
		out.AckAt = &a
	}
	if r.End != nil {
		e := *r.End
		out.End = &e
	}
	if r.Downlink.FirstAudioAfterAnswerMs != nil {
		v := *r.Downlink.FirstAudioAfterAnswerMs
		out.Downlink.FirstAudioAfterAnswerMs = &v
	}
	return out
}

// addFrame folds one received frame into the energy buckets.
func (d *Downlink) addFrame(sinceInvite time.Duration, rms float64) {
	d.Frames++
	start := int(sinceInvite.Milliseconds()) / energyWindowMs * energyWindowMs
	if n := len(d.Energy); n == 0 || d.Energy[n-1].StartMs != start {
		d.Energy = append(d.Energy, EnergyBucket{StartMs: start})
	}
	b := &d.Energy[len(d.Energy)-1]
	b.Frames++
	b.sum += rms
	b.MeanRMS = b.sum / float64(b.Frames)
	if rms > b.PeakRMS {
		b.PeakRMS = rms
	}
}
