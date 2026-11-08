// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
	"github.com/rasonyang/ai-native-callcenter/internal/voice"
)

type claimsFlows struct{ spec *flow.Spec }

func (f claimsFlows) PublishedSpec(context.Context, uuid.UUID) (*flow.Spec, error) {
	return f.spec, nil
}

type logLine struct {
	raw   string
	attrs map[string]any
}

func (l logLine) msg() string { s, _ := l.attrs["msg"].(string); return s }

func parseLogLines(t *testing.T, b *syncBuffer) []logLine {
	t.Helper()
	var out []logLine
	for raw := range strings.SplitSeq(b.String(), "\n") {
		if raw == "" {
			continue
		}
		l := logLine{raw: raw, attrs: map[string]any{}}
		if err := json.Unmarshal([]byte(raw), &l.attrs); err != nil {
			t.Fatalf("not JSON: %q: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

func awaitLogLine(t *testing.T, b *syncBuffer, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range parseLogLines(t, b) {
			if l.msg() == msg {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q line; log:\n%s", msg, b.String())
}

// sendInvite drives a real UAS the way the switch does: INVITE, 200, ACK.
func sendInvite(t *testing.T, uasPort int, sipCallID string, aiccHeader []string) {
	t.Helper()
	extra := "X-Aicc-Did: 95001\r\nX-Aicc-Channel-Id: chan-1\r\n"
	for _, h := range aiccHeader {
		extra += h + "\r\n"
	}
	sendInviteWith(t, uasPort, sipCallID, extra)
}

// sendInviteWith is sendInvite with the whole of the custom headers given, as
// CRLF-terminated lines.
func sendInviteWith(t *testing.T, uasPort int, sipCallID, extra string) {
	t.Helper()
	sip, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	rtp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sip.Close(); rtp.Close() })
	uas := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: uasPort}
	sipPort := sip.LocalAddr().(*net.UDPAddr).Port
	rtpPort := rtp.LocalAddr().(*net.UDPAddr).Port

	sdp := "v=0\r\no=peer 1 1 IN IP4 127.0.0.1\r\ns=test\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
		fmt.Sprintf("m=audio %d RTP/AVP 0 101\r\n", rtpPort) +
		"a=rtpmap:0 PCMU/8000\r\na=rtpmap:101 telephone-event/8000\r\na=ptime:20\r\na=sendrecv\r\n"
	request := func(method, body string) {
		ct := ""
		if body != "" {
			ct = "Content-Type: application/sdp\r\n"
		}
		msg := fmt.Sprintf("%s sip:aicc@127.0.0.1 SIP/2.0\r\n"+
			"Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-%s\r\n"+
			"From: <sip:1001@127.0.0.1>;tag=peer-tag\r\nTo: <sip:aicc@127.0.0.1>\r\n"+
			"Call-ID: %s\r\nCSeq: 1 %s\r\nContact: <sip:1001@127.0.0.1:%d>\r\n%s%sContent-Length: %d\r\n\r\n%s",
			method, sipPort, method, sipCallID, method, sipPort, extra, ct, len(body), body)
		if _, err := sip.WriteToUDP([]byte(msg), uas); err != nil {
			t.Fatal(err)
		}
	}
	request("INVITE", sdp)
	buf := make([]byte, 65536)
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = sip.SetReadDeadline(deadline)
		n, _, err := sip.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no 200 OK: %v", err)
		}
		if strings.HasPrefix(string(buf[:n]), "SIP/2.0 200") {
			break
		}
	}
	request("ACK", "")
}

// A bot leg whose INVITE carries no usable call id is given one by the
// orchestrator. From the mint on, every line about the dialog (the
// orchestrator's per-call lines, the session's, the UAS's and the media
// sessions') carries it as callId; lines before it carry sipCallId alone; and
// callId never holds the SIP Call-ID. A usable header needs no mint and is on
// every line from the INVITE.
func TestTheOrchestratorsFallbackIdReachesTheDialogsLaterLines(t *testing.T) {
	v7 := "01a19507-5177-7dc9-bef3-91e32915b768"
	for _, tc := range []struct {
		name   string
		sip    string
		header []string
		minted bool
	}{
		{"no header", "58e41e83-5042-1240-2298-610aa82ba9f6", []string{}, true},
		{"garbage header", "local@host", []string{"X-AICC-Call-ID: nope"}, true},
		{"nil uuid header", "a+b/c@host", []string{"X-AICC-Call-ID: " + uuid.Nil.String()}, true},
		{"v7 header", "58e41e83-5042-1240-2298-610aa82ba9f6", []string{"X-AICC-Call-ID: " + v7}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := flow.Load([]byte(claimsFlow))
			if err != nil {
				t.Fatal(err)
			}
			flowID := uuid.Must(uuid.NewV7())
			logs := &syncBuffer{}
			uasCfg := voice.DefaultConfig()
			uasCfg.SIPHost, uasCfg.SIPPort = "127.0.0.1", 0
			o, err := NewOrchestrator(OrchestratorConfig{
				Catalog: &fakeCatalog{dids: []catalog.DID{{
					ID: uuid.Must(uuid.NewV7()), Number: "95001", Language: "en",
					FlowID: &flowID, IsEnabled: true,
				}}},
				Flows:   claimsFlows{spec},
				Switch:  &fakeSwitch{},
				Profile: provider.OpenAIProfile(),
				UAS:     uasCfg,
				Sessions: func(provider.Profile, *slog.Logger) (provider.VoiceSession, error) {
					return newFakeModel(), nil
				},
				Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(o.Stop)

			sendInvite(t, o.uas.LocalPort(), tc.sip, tc.header)
			awaitLogLine(t, logs, "ai conversation started")
			o.Stop()
			awaitLogLine(t, logs, "sip bye sent")

			lines := parseLogLines(t, logs)
			var mintedID string
			for _, l := range lines {
				if l.msg() == "bot leg carried no parseable call id; minted one" {
					mintedID, _ = l.attrs["callId"].(string)
				}
			}
			if tc.minted {
				if _, err := uuid.Parse(mintedID); err != nil {
					t.Fatalf("no minted id on the mint warning: %q", mintedID)
				}
			} else if mintedID != "" {
				t.Fatalf("a usable header was replaced by a mint: %s", mintedID)
			}
			want := v7
			if tc.minted {
				want = mintedID
			}

			after := 0
			boundFrom := map[string]bool{"ai conversation started": true, "sip bye sent": true,
				"ai call bridged": true}
			for _, l := range lines {
				sip, _ := l.attrs["sipCallId"].(string)
				if m := l.msg(); m == "sip uas started" || m == "sip uas stopped" || sip == "" {
					if sip == "" && !strings.HasPrefix(l.msg(), "sip uas") {
						t.Errorf("a line about the call has no sipCallId: %s", l.raw)
					}
					continue
				}
				if sip != tc.sip {
					t.Errorf("sipCallId = %q, want %q: %s", sip, tc.sip, l.raw)
				}
				got, has := l.attrs["callId"].(string)
				if has && got == tc.sip {
					t.Errorf("callId holds the SIP Call-ID: %s", l.raw)
				}
				if strings.Count(l.raw, `"callId":`) > 1 || strings.Count(l.raw, `"sipCallId":`) > 1 {
					t.Errorf("a key is written twice: %s", l.raw)
				}
				if strings.Contains(l.raw, "aiccCallId") {
					t.Errorf("the redundant aiccCallId is back: %s", l.raw)
				}
				isBefore := tc.minted && (l.msg() == "invite accepted" || l.msg() == "rtp started")
				switch {
				case isBefore:
					if has {
						t.Errorf("%q was logged before any id was known but carries callId: %s", l.msg(), l.raw)
					}
				default:
					if got != want {
						t.Errorf("%q: callId = %q, want %q", l.msg(), got, want)
					}
					after++
				}
				delete(boundFrom, l.msg())
			}
			for m := range boundFrom {
				t.Errorf("no %q line to check", m)
			}
			if after < 3 {
				t.Errorf("only %d lines checked after the id was known", after)
			}
		})
	}
}
