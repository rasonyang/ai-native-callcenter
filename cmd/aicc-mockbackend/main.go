// SPDX-License-Identifier: Apache-2.0

// Command aicc-mockbackend serves the business APIs the reference flows call,
// so a bot can be walked end to end without a real CRM behind it.
//
// The five flows ported from the reference set (internal/seed/flows) all reach
// a backend for their facts — a repair order, an overdue bill, an appointment
// slot. Without one every tool fails, and a failing tool is the *correct*
// behaviour for a bot that must not invent facts, which makes it useless for
// checking whether the bot can hold a conversation. This supplies the facts.
//
// Fixtures are deliberately the reference ones, so a walkthrough has a
// determinate right answer: RMA1001 is in repair, 92223333 is 8 days overdue,
// 10086002 is inside its contract and cannot change plan. Somebody reading the
// walkthrough can tell "the bot said the wrong thing" from "the bot said what
// the data says".
//
//	go run ./cmd/aicc-mockbackend -addr 127.0.0.1:8770
//	AICC_BOT_BACKEND_BASE=http://127.0.0.1:8770 /tmp/aicc
//
// State lives in memory and only where a flow needs to observe its own writes
// (an appointment booked, then rescheduled, then confirmed). Restarting resets
// it, which is what a walkthrough wants: every run starts from the same facts.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The envelope every response carries. retCode "000000" is what the flows'
// successWhen tests against; anything else is a business failure the bot is
// expected to speak about rather than crash on.
const (
	codeOK   = "000000"
	codeFail = "500000"
)

type store struct {
	mu sync.Mutex
	// appointments by phone. One is pre-seeded so the "manage an existing
	// booking" branch has something to find on a fresh start.
	appointments map[string]*appointment
	apptSeq      int
	orderSeq     int
	// promises and leads are written and never read: the flows only need the
	// write to succeed. They are kept so a walkthrough can see arrivals in the
	// log rather than take the response on trust.
	promises int
	leads    int
}

type appointment struct {
	ID        string
	Item      string
	Slot      string
	Address   string
	Status    string
	Confirmed bool
}

func newStore() *store {
	return &store{
		appointments: map[string]*appointment{
			"91234567": {
				ID:      "AP1999",
				Item:    "星讯 X1 手机屏幕维修",
				Slot:    "明天上午 9点到12点",
				Address: "湖畔花园 3 栋 502",
				Status:  "booked",
			},
		},
		apptSeq:  2000,
		orderSeq: 12000,
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8770", "listen address")
	flag.Parse()

	s := newStore()
	mux := http.NewServeMux()
	for path, handler := range map[string]func(map[string]any) (any, string){
		"/api/repair/status":   s.repairStatus,
		"/api/appt/get":        s.apptGet,
		"/api/appt/slots":      s.apptSlots,
		"/api/appt/book":       s.apptBook,
		"/api/appt/reschedule": s.apptReschedule,
		"/api/appt/confirm":    s.apptConfirm,
		"/api/bb/account":      s.bbAccount,
		"/api/bb/verify":       s.bbVerify,
		"/api/bb/plans":        s.bbPlans,
		"/api/bb/change":       s.bbChange,
		"/api/bill/overdue":    s.billOverdue,
		"/api/bill/promise":    s.billPromise,
		"/api/bill/dispute":    s.billDispute,
		"/api/lead/submit":     s.leadSubmit,
	} {
		mux.HandleFunc(path, serve(path, handler))
	}

	log.Printf("mock backend on http://%s — point AICC_BOT_BACKEND_BASE at it", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// serve reads the request body, logs the exchange and writes the envelope.
//
// Every call is logged with its arguments, because half of what a walkthrough
// needs to know is whether the bot called the tool at all — and with what. A
// bot that asked for the wrong repair order and a bot that never asked look
// identical from the caller's side of the phone.
func serve(path string, handler func(map[string]any) (any, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body)
		}
		data, failure := handler(body)

		out := map[string]any{"retCode": codeOK, "retMsg": "Success"}
		if failure != "" {
			out = map[string]any{"retCode": codeFail, "retMsg": failure}
			log.Printf("%s %v → %s %s", path, body, codeFail, failure)
		} else {
			out["data"] = data
			log.Printf("%s %v → %v", path, body, data)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func toInt(v any) int {
	n, _ := strconv.Atoi(strings.TrimSpace(str(v)))
	return n
}

// --------------------------------------------------- 手机售后：查维修进度

func (s *store) repairStatus(b map[string]any) (any, string) {
	switch strings.ToUpper(str(b["rmaNo"])) {
	case "RMA1001":
		return map[string]any{"found": "1", "device": "星讯 X1 手机",
			"status": "维修中，已更换屏幕总成，正在整机测试", "eta": "预计2个工作日内寄出"}, ""
	case "RMA1002":
		return map[string]any{"found": "1", "device": "星讯 Air 手机",
			"status": "已维修完成并寄出", "eta": "快递单号尾号 6688，预计明天送达"}, ""
	}
	// found "0" rather than a failure: "no such repair order" is a fact the
	// bot should speak, not an error it should apologise for.
	return map[string]any{"found": "0", "device": "", "status": "", "eta": ""}, ""
}

// --------------------------------------------------- 上门服务：预约/改期/确认

// slotText renders a bookable window in the words a bot can read aloud.
func slotText(choice int) string {
	day := time.Now().AddDate(0, 0, 1)
	if choice == 2 {
		day = time.Now().AddDate(0, 0, 2)
		return fmt.Sprintf("%d月%d日下午 2点到5点", day.Month(), day.Day())
	}
	return fmt.Sprintf("%d月%d日上午 9点到12点", day.Month(), day.Day())
}

func (s *store) apptGet(b map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appt := s.appointments[str(b["phone"])]
	if appt == nil || appt.Status != "booked" {
		return map[string]any{"found": "0"}, ""
	}
	return map[string]any{"found": "1", "appt_id": appt.ID, "item": appt.Item,
		"slot": appt.Slot, "address": appt.Address}, ""
}

func (s *store) apptSlots(map[string]any) (any, string) {
	return map[string]any{"slot1": slotText(1), "slot2": slotText(2)}, ""
}

func (s *store) apptBook(b map[string]any) (any, string) {
	phone, choice := str(b["phone"]), toInt(b["date_choice"])
	if phone == "" || (choice != 1 && choice != 2) {
		return nil, "phone/date_choice required"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apptSeq++
	appt := &appointment{
		ID: fmt.Sprintf("AP%d", s.apptSeq), Item: str(b["item"]),
		Slot: slotText(choice), Address: str(b["address"]), Status: "booked",
	}
	s.appointments[phone] = appt
	return map[string]any{"appt_id": appt.ID, "slot": appt.Slot}, ""
}

func (s *store) apptReschedule(b map[string]any) (any, string) {
	choice := toInt(b["date_choice"])
	s.mu.Lock()
	defer s.mu.Unlock()
	appt := s.appointments[str(b["phone"])]
	if appt == nil || (choice != 1 && choice != 2) {
		return nil, "no appointment or bad date_choice"
	}
	appt.Slot = slotText(choice)
	return map[string]any{"appt_id": appt.ID, "slot": appt.Slot}, ""
}

func (s *store) apptConfirm(b map[string]any) (any, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appt := s.appointments[str(b["phone"])]
	if appt == nil {
		return nil, "no appointment"
	}
	appt.Confirmed = true
	return map[string]any{"confirmed": "1", "slot": appt.Slot}, ""
}

// --------------------------------------------------- 宽带套餐变更

func (s *store) bbAccount(b map[string]any) (any, string) {
	switch str(b["serviceNo"]) {
	case "10086001":
		return map[string]any{"found": "1", "name": "张女士",
			"current_plan": "100M 光纤，每月 68 元", "in_contract": "0"}, ""
	case "10086002":
		// In contract: the flow is expected to refuse the change and offer an
		// agent, which is the branch worth walking.
		return map[string]any{"found": "1", "name": "刘先生",
			"current_plan": "300M 光纤，每月 98 元（合约期内）", "in_contract": "1"}, ""
	}
	return map[string]any{"found": "0", "name": "", "current_plan": "", "in_contract": "0"}, ""
}

func (s *store) bbVerify(b map[string]any) (any, string) {
	last4 := map[string]string{"10086001": "3333", "10086002": "7777"}[str(b["serviceNo"])]
	match := "0"
	if last4 != "" && last4 == str(b["phoneLast4"]) {
		match = "1"
	}
	return map[string]any{"match": match}, ""
}

func (s *store) bbPlans(map[string]any) (any, string) {
	return map[string]any{
		"spoken": "套餐一，300M 光纤，每月 88 元；套餐二，500M 光纤，每月 108 元；" +
			"套餐三，千兆光纤，每月 158 元",
		"plan1": "300M 88元", "plan2": "500M 108元", "plan3": "1000M 158元",
	}, ""
}

func (s *store) bbChange(b map[string]any) (any, string) {
	plan := toInt(b["planId"])
	if str(b["serviceNo"]) == "" || plan < 1 || plan > 3 {
		return nil, "serviceNo/planId required"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orderSeq++
	return map[string]any{"order_no": fmt.Sprintf("BB-%d", s.orderSeq),
		"effective": "下一个账期生效"}, ""
}

// --------------------------------------------------- 账单提醒（早期催收）

func (s *store) billOverdue(b map[string]any) (any, string) {
	switch str(b["phone"]) {
	case "92223333":
		return map[string]any{"found": "1", "amount": "45.60",
			"due_date": "上月28日", "days_overdue": "8"}, ""
	case "90000000":
		return map[string]any{"found": "1", "amount": "196.00",
			"due_date": "上月15日", "days_overdue": "21"}, ""
	}
	return map[string]any{"found": "0", "amount": "", "due_date": "", "days_overdue": ""}, ""
}

func (s *store) billPromise(b map[string]any) (any, string) {
	if str(b["payDate"]) == "" {
		return nil, "payDate required"
	}
	s.mu.Lock()
	s.promises++
	n := s.promises
	s.mu.Unlock()
	log.Printf("  付款安排已记录，累计 %d 条", n)
	return map[string]any{"recorded": "1"}, ""
}

func (s *store) billDispute(map[string]any) (any, string) {
	s.mu.Lock()
	s.promises++
	s.mu.Unlock()
	return map[string]any{"recorded": "1"}, ""
}

// --------------------------------------------------- 销售线索

func (s *store) leadSubmit(b map[string]any) (any, string) {
	grade := map[string]string{"high": "A", "mid": "B", "low": "C"}[str(b["interest"])]
	if grade == "" {
		grade = "D"
	}
	s.mu.Lock()
	s.leads++
	n := s.leads
	s.mu.Unlock()
	log.Printf("  线索已记录，评级 %s，累计 %d 条", grade, n)
	return map[string]any{"lead_grade": grade, "recorded": "1"}, ""
}
