// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// slowStartModel is a model whose Start returns only when released, like a
// provider still waiting for its session.updated.
type slowStartModel struct {
	*fakeModel
	release chan struct{}
}

func (m *slowStartModel) Start(ctx context.Context, cfg provider.SessionConfig) error {
	select {
	case <-m.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return m.fakeModel.Start(ctx, cfg)
}

// Caller audio is never handed to the provider before Start returns. Grok
// leaves a session deaf if audio arrives before its configuration is
// acknowledged (docs/design/grok-findings.md section 2), and a trunk that opens
// media early puts frames on the leg while the model is still starting.
func TestNoCallerAudioReachesTheModelBeforeItsStartReturns(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	model := &slowStartModel{fakeModel: newFakeModel(), release: make(chan struct{})}
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		BargeGuard: -1, NoInput: -1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })

	started := make(chan error, 1)
	go func() { started <- session.Start(t.Context()) }()

	// Three frames: within the uplink's 100 ms backlog, so none is dropped.
	for i := range 3 {
		leg.frames <- callerFrame(media.LawMu, byte(0x40+i))
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(model.receivedAudio()); got != 0 {
		t.Fatalf("%d frames reached the model while it was still starting", got)
	}

	close(model.release)
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	// The uplink clock sends one frame per 20 ms, and a slot with nothing
	// queued yet carries a fill; the queued frames arrive in order among them.
	var heard []byte
	deadline := time.Now().Add(5 * time.Second)
	for len(heard) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		heard = heard[:0]
		for _, frame := range model.receivedAudio() {
			if frame[0] != media.SilenceFrame(media.LawMu)[0] {
				heard = append(heard, frame[0])
			}
		}
	}
	if string(heard) != "\x40\x41\x42" {
		t.Errorf("the model heard real frames %#x after Start, want the 3 queued in order", heard)
	}
}
