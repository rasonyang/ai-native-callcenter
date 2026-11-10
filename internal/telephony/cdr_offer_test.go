// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

type fixedStorage struct {
	size int64
	err  error
}

func (fixedStorage) Backend() string                                 { return "FS" }
func (fixedStorage) Bucket() string                                  { return "" }
func (s fixedStorage) Ingest(context.Context, string) (int64, error) { return s.size, s.err }
func (fixedStorage) Release(context.Context, string) error           { return nil }
func (fixedStorage) Pending(context.Context, time.Time) ([]string, error) {
	return nil, nil
}

type offerRecorder struct {
	got chan offered
}
type offered struct {
	rec      store.Recording
	bot, hum bool
}

func (o offerRecorder) OfferRecording(rec store.Recording, bot, human bool) {
	o.got <- offered{rec, bot, human}
}

func recordedCall(parties ...PartySnapshot) Snapshot {
	return Snapshot{CallID: uuid.Must(uuid.NewV7()), CallType: events.CallTypeInbound, CreatedAt: at(0), EndedAt: atPtr(60), Parties: parties}
}

// Post-call transcription is told about a booked recording, and about who
// answered on the far end, which is what its live-transcript rule needs.
func TestABookedRecordingIsOfferedWithWhoAnswered(t *testing.T) {
	t.Parallel()
	agent := uuid.Must(uuid.NewV7())
	caller := PartySnapshot{Role: RoleOriginator, Number: "13800138000", AnsweredAt: atPtr(0)}
	bot := PartySnapshot{Role: RoleTarget, IsBotLeg: true, AnsweredAt: atPtr(0)}
	human := PartySnapshot{Role: RoleTarget, AgentID: &agent, AnsweredAt: atPtr(30)}
	ringingAgent := PartySnapshot{Role: RoleTarget, AgentID: &agent} // never answered

	cases := []struct {
		name     string
		snap     Snapshot
		bot, hum bool
	}{
		{"bot only", recordedCall(caller, bot), true, false},
		{"agent only", recordedCall(caller, human), false, true},
		{"bot then agent", recordedCall(caller, bot, human), true, true},
		{"an agent who never answered is not a speaker", recordedCall(caller, bot, ringingAgent), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewCDRAssembler(&memoryLedger{}, staticQueues{}, fixedStorage{size: 64044}, nil)
			a.flushWait = time.Millisecond
			rec := offerRecorder{got: make(chan offered, 1)}
			a.SetRecordingOffer(rec)
			a.CallFinished(tc.snap)

			select {
			case o := <-rec.got:
				if o.bot != tc.bot || o.hum != tc.hum {
					t.Errorf("offered bot=%v human=%v, want bot=%v human=%v", o.bot, o.hum, tc.bot, tc.hum)
				}
				if o.rec.CallID != tc.snap.CallID || o.rec.DurationSec != 2 {
					t.Errorf("offered %+v", o.rec)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the recording was never offered")
			}
		})
	}
}

func TestNothingIsOfferedWhenThereIsNoRecording(t *testing.T) {
	t.Parallel()
	a := NewCDRAssembler(&memoryLedger{}, staticQueues{}, fixedStorage{err: errors.New("no file")}, nil)
	a.flushWait = time.Millisecond
	rec := offerRecorder{got: make(chan offered, 1)}
	a.SetRecordingOffer(rec)
	a.CallFinished(recordedCall(PartySnapshot{Role: RoleOriginator, AnsweredAt: atPtr(0)}))
	select {
	case o := <-rec.got:
		t.Fatalf("offered %+v for a call with no recording", o)
	case <-time.After(150 * time.Millisecond):
	}
}
