// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// The caller's number in these tests. Its digits differ from every number a
// case gives, so a log carrying it, or a callback falling back to it, is told
// apart from one carrying the given number.
const takeMessageCaller = "13900139000"

// takeMessageActions is the part of a call take_message needs: the
// deployment's dial settings, a ledger, the call's facts and a logger that
// writes into the returned buffer.
func takeMessageActions(countryCode string, form phonenum.DialForm) (*callActions, *fakeLedger, *bytes.Buffer) {
	ledger := newFakeLedger()
	logs := &bytes.Buffer{}
	facts := testFacts()
	facts.fromNumber = takeMessageCaller
	return &callActions{
		orchestrator: &Orchestrator{cfg: OrchestratorConfig{
			Ledger:             ledger,
			DefaultCountryCode: countryCode,
			DialForm:           form,
		}},
		log:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		recorder: newCallRecorder(uuid.Must(uuid.NewV7()), time.Now(), nil),
		facts:    facts,
	}, ledger, logs
}

// A callback number is kept only when it is a telephone number, written the
// way the deployment dials; anything else the model wrote is discarded for the
// caller's number, and the model is never refused (#311). Neither the message
// nor any number reaches the log.
func TestTakeMessageKeepsOnlyACallbackNumberThatIsANumber(t *testing.T) {
	t.Parallel()
	const message = "宽带每天晚上八点断线，请技术回电"
	cases := []struct {
		name, given string
		cc          string
		form        phonenum.DialForm
		want        string
		wantSource  string
		isWarned    bool
	}{
		{"none given", "", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, false},
		{"a description of the caller's number", "当前来电号码", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"this phone, in Chinese", "本机号码", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"this number", "this number", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"whitespace only", "   ", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"one digit short", "1380013800", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"an extension after the number", "13800138000转123", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"forty digits", strings.Repeat("1234567890", 4), "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"digits inside Chinese words", "号码是13800138000谢谢", "86", phonenum.FormNational, takeMessageCaller, callbackNumberCaller, true},
		{"spaced", "138 0013 8000", "86", phonenum.FormNational, "13800138000", callbackNumberSpoken, false},
		{"dashed", "138-0013-8000", "86", phonenum.FormNational, "13800138000", callbackNumberSpoken, false},
		{"a landline in brackets", "(010) 6123 4567", "86", phonenum.FormNational, "01061234567", callbackNumberSpoken, false},
		{"plus 86", "+86 13800138000", "86", phonenum.FormNational, "13800138000", callbackNumberSpoken, false},
		{"86 without a plus", "8613800138000", "86", phonenum.FormNational, "13800138000", callbackNumberSpoken, false},
		{"full-width digits", "１３８００１３８０００", "86", phonenum.FormNational, "13800138000", callbackNumberSpoken, false},
		{"a mobile under E164", "138 0013 8000", "86", phonenum.FormE164, "+8613800138000", callbackNumberSpoken, false},
		{"a landline under E164", "(010) 6123 4567", "86", phonenum.FormE164, "+861061234567", callbackNumberSpoken, false},
		{"a mobile under E164_NO_PLUS", "+86 13800138000", "86", phonenum.FormE164NoPlus, "8613800138000", callbackNumberSpoken, false},
		{"a foreign number under NATIONAL", "+1 (415) 555-0100", "86", phonenum.FormNational, "+14155550100", callbackNumberSpoken, false},
		{"no dial settings at all", "138-0013-8000", "", "", "13800138000", callbackNumberSpoken, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			actions, ledger, logs := takeMessageActions(c.cc, c.form)

			result, err := actions.TakeMessage(t.Context(),
				flow.MessageRequest{Message: message, CallbackNumber: c.given})
			if err != nil || !result.IsOK {
				t.Fatalf("take_message refused: %+v %v; a callback number is never a reason to refuse", result, err)
			}
			if len(ledger.callbacks) != 1 {
				t.Fatalf("saved %d callbacks, want 1", len(ledger.callbacks))
			}
			if got := ledger.callbacks[0].PhoneNumber; got != c.want {
				t.Errorf("phone_number = %q, want %q", got, c.want)
			}
			if got := ledger.callbacks[0].Message; got != message {
				t.Errorf("message = %q, want it stored as given", got)
			}

			out := logs.String()
			for _, secret := range logSecrets(message, c.given, c.want) {
				if strings.Contains(out, secret) {
					t.Errorf("the log carries %q:\n%s", secret, out)
				}
			}
			if !strings.Contains(out, "callbackNumberSource="+c.wantSource) {
				t.Errorf("the log does not say the number is %s:\n%s", c.wantSource, out)
			}
			if isWarned := strings.Contains(out, "level=WARN"); isWarned != c.isWarned {
				t.Errorf("warned = %v, want %v:\n%s", isWarned, c.isWarned, out)
			}
		})
	}
}

// logSecrets is what must never appear in the log: the message, the value the
// model gave, its digits, and the stored number, which is masked to its last
// four digits.
func logSecrets(message, given, stored string) []string {
	secrets := []string{message, takeMessageCaller, stored[:len(stored)-4]}
	if strings.TrimSpace(given) != "" {
		secrets = append(secrets, given)
	}
	digits := strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, given)
	if len(digits) > 4 {
		secrets = append(secrets, digits)
	}
	return secrets
}

// A ledger that cannot save is still a conversation: the model is told, and
// the number it gave does not reach the error log either.
func TestTakeMessageSaveFailureKeepsTheNumberOutOfTheLog(t *testing.T) {
	t.Parallel()
	actions, _, logs := takeMessageActions("86", phonenum.FormNational)
	actions.orchestrator.cfg.Ledger = failingLedger{}

	result, err := actions.TakeMessage(t.Context(),
		flow.MessageRequest{Message: "请回电", CallbackNumber: "138 0013 8000"})
	if err != nil || result.IsOK {
		t.Fatalf("result = %+v, %v; want a refusal the model can read", result, err)
	}
	if out := logs.String(); strings.Contains(out, "13800138000") || strings.Contains(out, "请回电") {
		t.Errorf("the log carries the number or the message:\n%s", out)
	}
}

// failingLedger refuses every write.
type failingLedger struct{}

func (failingLedger) InsertCDR(context.Context, store.CDR) error {
	return errors.New("database is down")
}

func (failingLedger) InsertCallback(context.Context, *uuid.UUID, *uuid.UUID, string, string) (store.Callback, error) {
	return store.Callback{}, errors.New("database is down")
}

func TestMaskNumber(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":               "",
		"1002":           "***",
		"13800138000":    "***8000",
		"+8613800138000": "***8000",
	} {
		if got := maskNumber(in); got != want {
			t.Errorf("maskNumber(%q) = %q, want %q", in, got, want)
		}
	}
}
