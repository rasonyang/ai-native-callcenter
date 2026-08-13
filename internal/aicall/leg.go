// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

// Leg is the telephone side of a bridged call: audio in fixed frames, digits,
// and an ending.
//
// The bridge is written against this rather than against a SIP dialog because
// nothing it does depends on SIP. Keeping the seam here also means the audio
// path can be exercised without a signalling stack underneath it.
type Leg interface {
	// ID identifies the call in logs.
	ID() string
	// Law is the companding in use, which decides what conversion the model
	// side needs.
	Law() media.Law
	// Frames yields inbound audio, one 20 ms frame at a time. Each frame is
	// returned to the media pool by the consumer.
	Frames() <-chan []byte
	// Digits yields keypresses, however they arrived.
	Digits() <-chan string
	// Send queues one frame of outbound audio, reporting false when the queue
	// is full.
	Send(frame []byte) bool
	// ClearTx drops queued audio and reports how many frames went with it.
	ClearTx() int
	// Pending is how many frames are queued but not yet on the wire. Audio
	// having been generated is not the same as the caller having heard it, and
	// anything that must happen after the caller hears something waits on this.
	Pending() int
	// Stopped closes when the call ends, from either side.
	Stopped() <-chan struct{}
	// Stop ends the call.
	Stop()
}

// dialogLeg adapts a SIP dialog to the bridge.
type dialogLeg struct{ dialog *voice.Dialog }

// FromDialog wraps an accepted SIP dialog as a leg.
func FromDialog(dialog *voice.Dialog) Leg { return dialogLeg{dialog} }

func (l dialogLeg) ID() string               { return l.dialog.CallID }
func (l dialogLeg) Law() media.Law           { return l.dialog.RTP.Law() }
func (l dialogLeg) Frames() <-chan []byte    { return l.dialog.RTP.Frames() }
func (l dialogLeg) Digits() <-chan string    { return l.dialog.RTP.DTMF() }
func (l dialogLeg) Send(frame []byte) bool   { return l.dialog.RTP.Send(frame) }
func (l dialogLeg) ClearTx() int             { return l.dialog.RTP.ClearTx() }
func (l dialogLeg) Pending() int             { return l.dialog.RTP.Pending() }
func (l dialogLeg) Stopped() <-chan struct{} { return l.dialog.Stopped }
func (l dialogLeg) Stop()                    { l.dialog.Stop() }
