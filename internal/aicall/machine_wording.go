// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
)

// Machine wording (#409, phase 1). A call the platform places to a customer
// may be answered by a machine: a voicemail prompt, a phone's or a carrier's
// call assistant, a carrier announcement. A call assistant answers in natural
// sentences and reacts to what the bot says ("好的，我会转告他"), so a model asked
// whether the customer agreed hears agreement, and on a campaign deployment
// such calls recorded HIGH and MEDIUM leads. So on such a call every final
// caller transcript is checked against a built-in list of wording a machine
// says and a person answering their own phone does not, and a match marks the
// call as answered by a machine for the rest of its life (noteMachineWording,
// told of each final before anything waiting for it is). The lead guard
// (lead_guard.go) then refuses every tool call that would record the
// customer's interest. The call goes on; ending it is a later phase's.
//
// Only a call placed to a customer is judged (Config.IsPlacedToCustomer): a
// caller who dials in and asks to leave a message is a person. A call with no
// caller transcript, or a blank one, is never marked, and neither is wording
// outside the list, which grows from labelled transcripts.

// machineWording is the built-in list, by kind of machine, in the order a
// match is looked for: the call assistant's wording first, then the
// voicemail's, then the carrier's, because a call assistant and a voicemail
// often begin with the carrier's words ("您拨打的用户暂时无法接通，请在提示音后
// 留言"). Every phrase is in compact form (compactWording) and is matched
// anywhere inside one clause of the transcript (machineClauses). Every
// language's phrases count on every call: a carrier in China plays its
// announcement in Chinese and then in English, and a voicemail greeting is in
// the language its owner recorded.
//
// The list is conservative: a phrase a person plausibly says on their own
// phone is not one, whatever a machine also says it in. So not bare 转告 or 留言
// ("我会转告他" is what a relative says), not bare 机主 ("我是机主" answers a
// question a telecom offer may ask), not bare 智能助手 or "virtual assistant"
// ("你是智能助手吗" is a person asking about the bot), not bare 空号, 已停机 or
// 不在服务区 (a callee talking about a number card says them), not bare
// 正在通话中 ("我正在通话中"), not bare "mailbox" (an email box) and nothing
// with "screening" but "screening service".
var machineWording = []struct {
	category string
	phrases  []string
}{
	{obs.MachineCallAssistant, []string{
		// The owner of the line, in the third person.
		"机主的", "机主现在", "机主暂时", "机主目前", "机主正在", "机主不方便", "机主无法", "机主稍后",
		"机主已开启", "转告机主", "转告给机主", "转达机主", "转达给机主", "转给机主", "转到机主", "转交机主",
		"通知机主", "告知机主", "告诉机主", "提醒机主",
		// The callee-side products by name, and an assistant introducing
		// itself.
		"来电助理", "来电助手", "通话助理", "通话助手",
		"我是智能助理", "我是智能助手", "我是语音助理", "我是语音助手", "我是ai助理", "我是ai助手",
		"screeningservice", "iamavirtualassistant", "imavirtualassistant", "thisisavirtualassistant",
		"iamacallassistant", "imacallassistant", "ifthispersonisavailable",
		"yournameandwhyyourecalling", "yournameandreasonforcalling",
	}},
	{obs.MachineVoicemail, []string{
		"提示音后", "嘀声后", "滴声后", "哔声后", "嘟声后", "声后留言", "录制留言", "请留言", "语音信箱",
		"留言信箱",
		"leaveamessage", "leaveyourmessage", "recordyourmessage", "afterthetone", "afterthebeep",
		"atthetone", "voicemail", "mailboxisfull",
	}},
	{obs.MachineCarrierAnnouncement, []string{
		"您拨打的用户", "您拨打的号码", "您拨打的电话", "您所拨打的", "您呼叫的用户", "您呼叫的号码",
		"已关机", "暂时无法接通", "用户正在通话中", "请稍后再拨", "查证后再拨",
		// "dial" covers dialed and dialled.
		"thenumberyouhavedial", "thenumberyoudial", "thepersonyouarecalling", "thepersonyourecalling",
		"thesubscriberyou",
	}},
}

// classifyMachineWording reports whether a caller's final transcript carries
// wording only a machine says (machineWording), and if so the kind of machine
// (obs.Machine*) and the phrase that matched. The transcript is split into
// clauses at its punctuation and each is compacted, so the spaces and
// punctuation a transcriber adds inside a phrase, and full-width letters, do
// not hide it, while two clauses of a person's are not glued into one phrase
// ("我是机主，现在不方便" is not "机主现在"). Pure; it runs off the frame path.
func classifyMachineWording(text string) (category, phrase string, ok bool) {
	clauses := machineClauses(text)
	if len(clauses) == 0 {
		return "", "", false
	}
	for _, group := range machineWording {
		for _, clause := range clauses {
			for _, p := range group.phrases {
				if strings.Contains(clause, p) {
					return group.category, p, true
				}
			}
		}
	}
	return "", "", false
}

// machineClauses is a transcript's clauses in compact form (compactWording),
// split at sentence and clause punctuation, full-width or half-width; empty
// clauses are left out. An apostrophe is no boundary ("you're" stays one
// word).
func machineClauses(text string) []string {
	var clauses []string
	for _, piece := range strings.FieldsFunc(foldWidth(text), isClauseBreak) {
		if clause := compactWording(piece); clause != "" {
			clauses = append(clauses, clause)
		}
	}
	return clauses
}

// isClauseBreak says r ends a clause: a sentence terminator, a comma, an
// enumeration comma, a semicolon, a colon, an ellipsis or a line break, after
// foldWidth.
func isClauseBreak(r rune) bool {
	switch r {
	case '。', '.', '!', '?', ',', '、', ';', ':', '…', '\n', '\r':
		return true
	}
	return false
}

// compactWording is compactAck after foldWidth: letters and digits,
// lower-cased, Han characters, nothing else.
func compactWording(text string) string {
	return compactAck(foldWidth(text))
}

// foldWidth turns full-width ASCII forms (U+FF01 to U+FF5E: "ＡＢＣ", "，", "？")
// into their half-width letters and signs, so both read alike.
func foldWidth(text string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0xFF01 && r <= 0xFF5E {
			return r - 0xFEE0
		}
		return r
	}, text)
}

// noteMachineWording judges one final caller transcript for a machine's
// wording, on a call placed to a customer, and marks the call when it carries
// some. The mark is the first match's and is never cleared; marking logs once
// and counts once. It is called before the final is recorded and told to any
// wait for it (noteCallerFinal), so a guard that waited for this utterance
// judges it with its mark.
func (s *Session) noteMachineWording(text string) {
	if !s.cfg.IsPlacedToCustomer || strings.TrimSpace(text) == "" {
		return
	}
	category, phrase, ok := classifyMachineWording(text)
	if !ok {
		return
	}
	s.ct.mu.Lock()
	isFirst := s.ct.machineCategory == ""
	if isFirst {
		s.ct.machineCategory = category
	}
	s.ct.mu.Unlock()
	if !isFirst {
		return
	}
	s.log.Info("the call is marked as answered by a machine; it records no interest from here on",
		"category", category, "phrase", phrase)
	obs.RecordMachineAnswered(category, s.providerName)
}

// machineAnswered is the kind of machine whose wording marked the call, and
// whether it is marked (noteMachineWording).
func (s *Session) machineAnswered() (category string, isMarked bool) {
	s.ct.mu.Lock()
	defer s.ct.mu.Unlock()
	return s.ct.machineCategory, s.ct.machineCategory != ""
}
