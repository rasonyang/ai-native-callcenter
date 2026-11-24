// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// #409: the classifier of a machine's wording. Every fixture is synthetic.
func TestTheMachineWordingClassifier(t *testing.T) {
	for _, tc := range []struct {
		text, want string
	}{
		// Voicemail prompts.
		{"请在提示音后录制留言", obs.MachineVoicemail},
		{"请在提示音后，录制留言。", obs.MachineVoicemail},
		{"请在 提示音 后 录制留言", obs.MachineVoicemail},
		{"您好，机器暂时无人接听，请在嘀一声后留言。", obs.MachineVoicemail},
		{"please leave a message after the tone", obs.MachineVoicemail},
		{"Please leave a message, after the tone.", obs.MachineVoicemail},
		{"PLEASE LEAVE A MESSAGE AFTER THE BEEP", obs.MachineVoicemail},
		{"ＰＬＥＡＳＥ　ＬＥＡＶＥ　Ａ　ＭＥＳＳＡＧＥ", obs.MachineVoicemail},
		{"You have reached the voice mail of extension 2201.", obs.MachineVoicemail},
		// Call assistants.
		{"我是机主的智能助理，机主现在不方便接听", obs.MachineCallAssistant},
		{"我是机主的智能助理,机主现在不方便接听。", obs.MachineCallAssistant},
		{"我稍后会将您的来电情况转到机主", obs.MachineCallAssistant},
		{"我稍后会将您的来电情况，转到机主。", obs.MachineCallAssistant},
		{"您好，我是他的来电助理，请问您有什么事？", obs.MachineCallAssistant},
		{"我是AI助理，您可以告诉我您的来意。", obs.MachineCallAssistant},
		{"Hi, the person you're calling is using a screening service.", obs.MachineCallAssistant},
		// A call assistant that begins with the carrier's words is a call
		// assistant; a voicemail that does is a voicemail.
		{"您拨打的用户暂时无法接听，我是机主的语音助手。", obs.MachineCallAssistant},
		{"您拨打的用户暂时无法接通，请在提示音后留言。", obs.MachineVoicemail},
		// Carrier announcements.
		{"您拨打的用户已关机", obs.MachineCarrierAnnouncement},
		{"您拨打的用户已关机。Sorry, the subscriber you dialed is power off.", obs.MachineCarrierAnnouncement},
		{"您拨打的号码是空号", obs.MachineCarrierAnnouncement},
		{"您拨打的号码是空号，请查证后再拨。", obs.MachineCarrierAnnouncement},
		{"暂时无法接通", obs.MachineCarrierAnnouncement},
		{"对不起，您拨打的电话暂时无法接通，请稍后再拨。", obs.MachineCarrierAnnouncement},
		{"Sorry, the number you have dialled is not in service.", obs.MachineCarrierAnnouncement},
		{"The person you are calling cannot be reached.", obs.MachineCarrierAnnouncement},

		// People.
		{"喂？喂？", ""},
		{"您好，哪位？", ""},
		{"您好，这里是某某公司", ""},
		{"好的", ""},
		{"好的，我会转告他。", ""},
		{"不需要", ""},
		{"我在开会，晚点再说", ""},
		{"你是机器人吗", ""},
		{"你是语音助手吗？", ""},
		{"你是不是智能助理啊", ""},
		{"我是机主，现在不方便，晚点说。", ""},
		{"对，我就是机主本人。", ""},
		// What a callee says when a flow asks whether they own the line
		// (early_collections).
		{"我是机主,我不想聊这个。", ""},
		{"是我本人", ""},
		{"不好意思，我正在通话中，稍后打给你。", ""},
		{"这个号是空号吗？", ""},
		{"你可以给他留言。", ""},
		{"hello?", ""},
		{"who is this", ""},
		{"I'm in a meeting", ""},
		{"Are you a virtual assistant?", ""},
		{"Send it to my mailbox.", ""},
		{"", ""},
		{"。", ""},
	} {
		category, phrase, ok := classifyMachineWording(tc.text)
		if category != tc.want || ok != (tc.want != "") {
			t.Errorf("classifyMachineWording(%q) = %q (%q), %v; want %q", tc.text, category, phrase, ok, tc.want)
		}
		if ok && !strings.Contains(strings.Join(machineClauses(tc.text), "|"), phrase) {
			t.Errorf("classifyMachineWording(%q) names %q, which is not in it", tc.text, phrase)
		}
	}
}

// Every phrase of the list is in the compact form it is matched in, and no
// phrase is listed twice.
func TestEveryMachinePhraseIsCompact(t *testing.T) {
	seen := map[string]string{}
	for _, group := range machineWording {
		for _, p := range group.phrases {
			if p == "" || compactWording(p) != p {
				t.Errorf("%s phrase %q is not compact (%q)", group.category, p, compactWording(p))
			}
			if other, ok := seen[p]; ok {
				t.Errorf("phrase %q listed under %s and %s", p, other, group.category)
			}
			seen[p] = group.category
		}
	}
}

// Only a call placed to a customer is marked; the mark is the first match's,
// is never cleared, and is logged once.
func TestOnlyACallPlacedToACustomerIsMarkedAsAnsweredByAMachine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts *callFacts
		want  bool
	}{
		{"placed to a customer", &callFacts{callType: callTypeOutbound, isPlacedToCustomer: true}, true},
		{"inbound", &callFacts{callType: callTypeInbound}, false},
		{"internal", &callFacts{callType: callTypeInternal}, false},
		{"an agent's call to the bot's DID", &callFacts{callType: callTypeOutbound}, false},
		{"no facts", nil, false},
	} {
		if got := tc.facts.isOutboundToCustomer(); got != tc.want {
			t.Errorf("%s: isOutboundToCustomer = %v, want %v", tc.name, got, tc.want)
		}
	}

	logs := &syncBuffer{}
	s := &Session{cfg: Config{IsPlacedToCustomer: true}, log: slog.New(slog.NewTextHandler(logs, nil)),
		providerName: "qwen"}
	s.noteMachineWording("喂？")
	if _, isMarked := s.machineAnswered(); isMarked {
		t.Fatal("a person's greeting marked the call")
	}
	s.noteMachineWording("我是机主的智能助理，机主现在不方便接听。")
	s.noteMachineWording("请在提示音后录制留言。")
	s.noteMachineWording("好的。")
	if category, isMarked := s.machineAnswered(); !isMarked || category != obs.MachineCallAssistant {
		t.Errorf("mark = %q, %v; want %s, kept", category, isMarked, obs.MachineCallAssistant)
	}
	if got := strings.Count(logs.String(), "the call is marked as answered by a machine"); got != 1 {
		t.Errorf("%d mark lines, want one:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "category=CALL_ASSISTANT") {
		t.Errorf("the mark line lacks the category:\n%s", logs.String())
	}

	inbound := &Session{log: slog.New(slog.NewTextHandler(logs, nil)), providerName: "qwen"}
	inbound.noteMachineWording("请在提示音后录制留言。")
	if _, isMarked := inbound.machineAnswered(); isMarked {
		t.Error("an inbound call was marked")
	}
}
