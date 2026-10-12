// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// cdrBook is the ledger the assembler writes to.
type cdrBook struct {
	mu   sync.Mutex
	cdrs []store.CDR
}

func (b *cdrBook) InsertCDR(_ context.Context, c store.CDR) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cdrs = append(b.cdrs, c)
	return nil
}
func (b *cdrBook) InsertQueueEvent(context.Context, time.Time, *uuid.UUID, uuid.UUID, string, *uuid.UUID, int) error {
	return nil
}
func (b *cdrBook) InsertRecording(_ context.Context, r store.Recording) (store.Recording, error) {
	return r, nil
}
func (b *cdrBook) MarkRecorded(context.Context, uuid.UUID) error { return nil }

func (b *cdrBook) all() []store.CDR {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]store.CDR(nil), b.cdrs...)
}

// hangup delivers the switch's hangup for a channel it held, as the listing
// described it.
func (r *recoveryRig) hangup(t *testing.T, id, cause string, at time.Time) {
	t.Helper()
	var h map[string]string
	if err := json.Unmarshal([]byte(r.sw.dumps[id]), &h); err != nil {
		t.Fatal(err)
	}
	h["Event-Name"] = "CHANNEL_HANGUP_COMPLETE"
	h["Hangup-Cause"] = cause
	h["Event-Date-Timestamp"] = strconv.FormatInt(at.UnixMicro(), 10)
	ev, ok := telephony.Normalize(esl.NewEvent(h, ""))
	if !ok {
		t.Fatalf("hangup of %s did not normalize", id)
	}
	r.coord.Handle(context.Background(), ev)
}

// A bot call that outlived the process has no session to write its row. The
// bot's story died with the process, so the row falls to the telephony path;
// without it the call vanishes from the ledger (live 2026-10-12, call
// 01a12ee6): the reap ended both legs and nothing recorded the call.
func TestAReapedBotCallStillGetsItsCDR(t *testing.T) {
	for name, botFirst := range map[string]bool{"customer hangs up first": false, "bot leg hangs up first": true} {
		t.Run(name, func(t *testing.T) {
			rig := newRecoveryRig(t)
			book := &cdrBook{}
			rig.coord.AttachCDR(telephony.NewCDRAssembler(book, nil, nil,
				slog.New(slog.NewTextHandler(io.Discard, nil))))
			cust, bot := deadBot(rig, nil)

			rep := rig.reconcile(t)
			if rep.OrphanedBot != 1 {
				t.Fatalf("report = %+v, want the bot call reaped", rep)
			}

			end := t0.Add(95 * time.Second)
			first, second := cust, bot
			if botFirst {
				first, second = bot, cust
			}
			rig.hangup(t, first, ReconcileCause, end)
			rig.hangup(t, second, ReconcileCause, end.Add(10*time.Millisecond))

			deadline := time.Now().Add(2 * time.Second)
			for len(book.all()) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond) // a second row would show up by now
			got := book.all()
			if len(got) != 1 {
				t.Fatalf("%d CDRs written, want exactly 1", len(got))
			}
			cdr := got[0]
			if cdr.HangupCause != ReconcileCause {
				t.Errorf("hangupCause = %q, want %q", cdr.HangupCause, ReconcileCause)
			}
			if cdr.AnsweredAt.IsZero() {
				t.Errorf("answeredAt = %v, want the switch's answer time", cdr.AnsweredAt)
			}
			if len(cdr.Legs) != 1 || cdr.Legs[0].Kind != "BOT" || cdr.Legs[0].DurationSec <= 0 {
				t.Errorf("legs = %+v, want the bot's leg with its duration", cdr.Legs)
			}
		})
	}
}
