// SPDX-License-Identifier: Apache-2.0

package mockbackend

import (
	"encoding/json"
	"net/http"
	"strings"
)

// A stand-in for aicc-knowledge (github.com/rasonyang/aicc-knowledge), the
// retrieval service the reference flow docs/examples/flows/knowledge_support.json
// calls. It answers the two operations of that service's contract
// (docs/openapi.json: searchFaq and lookupFact) in the contract's shapes, with
// a few fixed fixtures, so the flow simulator can replay the flow without the
// service. It is not an envelope-style endpoint: success is a 200 with the
// contract's body, and every failure is {"error": {code, message, params}}.
//
// Fixtures:
//   - search: a query naming installation, moving house or payment is a HIT
//     in either language (EN/ZH index chosen by the request's language);
//     anything else is NO_MATCH. A query containing kbDownQuery answers 503
//     INDEX_UNAVAILABLE, which is how a scenario provokes "the service is
//     down" without bending the engine.
//   - facts: table "plans" only, rows as the live aicc-knowledge table holds
//     them (Basic, Plan 59, Plan 59 Plus, Plan 99, Fiber 500, Fiber 1000 and
//     Senior Care are FOUND; any other key, "Plan 79" for one, is NOT_FOUND).
//     "Senior Care" is deliberately absent from the flow's enum but present
//     here, as in a real table that has outgrown the flow.
//
// The bearer key is checked loosely: any non-empty Bearer value passes,
// because the simulator sends a placeholder secret. A missing header is 401
// UNAUTHORIZED.

// kbDownQuery makes /v1/search answer 503 INDEX_UNAVAILABLE.
const kbDownQuery = "kb-down"

type kbQA struct {
	keywords []string
	items    []kbItem
}

type kbItem struct {
	ID        string
	Question  string
	Answer    string
	SourceRef string
	Score     float64
}

var kbFAQ = map[string][]kbQA{
	"EN": {
		{[]string{"install"}, []kbItem{
			{"en-install-1", "How long does installation take?", "A technician installs the service within three working days after the order is confirmed and calls you first to agree a time.", "faq_en.docx#Installation/Timing", 0.91},
			{"en-install-2", "Do I need to be home for the installation?", "An adult needs to be home to let the technician in and to sign the work order.", "faq_en.docx#Installation/Visit", 0.84},
		}},
		{[]string{"move", "relocat"}, []kbItem{
			{"en-move-1", "Can I move my service to a new address?", "Yes. Ask for a relocation at least five working days before you move, and the new address is checked for coverage first.", "faq_en.docx#Relocation", 0.9},
		}},
		{[]string{"pay"}, []kbItem{
			{"en-pay-1", "How can I pay my bill?", "You can pay in the app, at a service counter, or by bank transfer using your service number as the reference.", "faq_en.docx#Billing/Payment", 0.88},
		}},
	},
	"ZH": {
		{[]string{"安装"}, []kbItem{
			{"zh-install-1", "安装需要多久?", "订单确认后三个工作日内上门安装,师傅会提前电话与您约定时间。", "faq_zh.docx#安装/时效", 0.91},
			{"zh-install-2", "安装时家里需要有人吗?", "安装时需要有成年人在家开门,并在工单上签字确认。", "faq_zh.docx#安装/上门", 0.84},
		}},
		{[]string{"搬家", "移机"}, []kbItem{
			{"zh-move-1", "搬家后宽带可以迁移到新地址吗?", "可以。请在搬家前至少五个工作日申请移机,我们会先核实新地址是否在覆盖范围内。", "faq_zh.docx#移机", 0.9},
		}},
		{[]string{"缴费", "交费"}, []kbItem{
			{"zh-pay-1", "怎么缴费?", "您可以在手机应用里缴费,也可以到营业厅柜台缴费,或者用业务号码作为备注银行转账。", "faq_zh.docx#账单/缴费", 0.88},
		}},
	},
}

// kbPlan is one row of the live "plans" fact table, in its shape: snake_case
// columns, DECIMAL as a JSON number, nil for a column the kind does not have.
type kbPlan struct {
	kind                    string
	nameEN, nameZH          string
	fee                     int
	dataGB, voice, speed    any
	contractMonths, termFee int
	validFrom               string
}

var kbPlans = map[string]kbPlan{
	"Basic":        {"MOBILE", "Basic", "基础套餐", 29, 5, 100, nil, 0, 0, "2025-01-01"},
	"Plan 59":      {"MOBILE", "Plan 59", "59元套餐", 59, 30, 300, nil, 12, 100, "2025-01-01"},
	"Plan 59 Plus": {"MOBILE", "Plan 59 Plus", "59元Plus套餐", 59, 60, 500, nil, 24, 200, "2025-01-01"},
	"Plan 99":      {"MOBILE", "Plan 99", "99元套餐", 99, 100, 1000, nil, 24, 300, "2025-01-01"},
	"Fiber 500":    {"BROADBAND", "Fiber 500", "光纤500", 129, nil, nil, 500, 12, 200, "2026-10-27"},
	"Fiber 1000":   {"BROADBAND", "Fiber 1000", "光纤1000", 199, nil, nil, 1000, 24, 400, "2025-01-01"},
	"Senior Care":  {"MOBILE", "Senior Care", "敬老套餐", 19, 3, 300, nil, 0, 0, "2025-01-01"},
}

func kbError(w http.ResponseWriter, status int, code, message string, params map[string]any) {
	if params == nil {
		params = map[string]any{}
	}
	writeEnvelope(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "params": params}})
}

func kbWriteJSON(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// kbGuard checks the method and the bearer header, and decodes the body.
func kbGuard(w http.ResponseWriter, r *http.Request, logf func(string, ...any)) (map[string]any, bool) {
	if r.Method != http.MethodPost {
		kbError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only", nil)
		return nil, false
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); !ok || strings.TrimSpace(token) == "" {
		logf("%s refused: no bearer key", r.URL.Path)
		kbError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid API key", nil)
		return nil, false
	}
	body := map[string]any{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		kbError(w, http.StatusBadRequest, "BAD_REQUEST", "body is not a JSON object", nil)
		return nil, false
	}
	return body, true
}

func kbSearch(logf func(string, ...any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := kbGuard(w, r, logf)
		if !ok {
			return
		}
		query, _ := body["query"].(string)
		language, _ := body["language"].(string)
		if strings.TrimSpace(query) == "" || (language != "EN" && language != "ZH") {
			kbError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "query and language are required",
				map[string]any{"invalid": []string{"query", "language"}})
			return
		}
		lower := strings.ToLower(query)
		if strings.Contains(lower, kbDownQuery) {
			logf("/v1/search %v → 503 INDEX_UNAVAILABLE", body)
			kbError(w, http.StatusServiceUnavailable, "INDEX_UNAVAILABLE", "no live index", map[string]any{"language": language})
			return
		}
		items := []map[string]any{}
		for _, qa := range kbFAQ[language] {
			for _, kw := range qa.keywords {
				if strings.Contains(lower, kw) {
					for _, it := range qa.items {
						items = append(items, map[string]any{"id": it.ID, "question": it.Question,
							"answer": it.Answer, "sourceRef": it.SourceRef, "score": it.Score})
					}
					break
				}
			}
			if len(items) > 0 {
				break
			}
		}
		status := "NO_MATCH"
		if len(items) > 0 {
			status = "HIT"
		}
		logf("/v1/search %v → %s (%d items)", body, status, len(items))
		kbWriteJSON(w, map[string]any{"status": status, "items": items, "latencyMs": 12})
	}
}

func kbLookupPlan(logf func(string, ...any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := kbGuard(w, r, logf)
		if !ok {
			return
		}
		key, _ := body["key"].(map[string]any)
		if _, hasAt := body["at"]; hasAt {
			// The reference flow never sends `at`; a scenario that does is wrong.
			kbError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "this mock refuses `at`", map[string]any{"unknown": []string{"at"}})
			return
		}
		plan, isString := key["plan"].(string)
		if len(key) != 1 || !isString {
			kbError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "key must be exactly {plan}", map[string]any{"missing": []string{"plan"}})
			return
		}
		row, found := kbPlans[plan]
		if !found {
			logf("/v1/facts/plans/lookup %v → NOT_FOUND", body)
			kbWriteJSON(w, map[string]any{"status": "NOT_FOUND", "row": nil, "sourceRef": nil, "validFrom": nil, "validTo": nil})
			return
		}
		logf("/v1/facts/plans/lookup %v → FOUND", body)
		kbWriteJSON(w, map[string]any{
			"status": "FOUND",
			"row": map[string]any{"contract_months": row.contractMonths, "data_gb": row.dataGB,
				"early_termination_fee": row.termFee, "kind": row.kind, "monthly_fee": row.fee,
				"name_en": row.nameEN, "name_zh": row.nameZH, "plan": plan, "speed_mbps": row.speed,
				"valid_from": row.validFrom, "valid_to": nil, "voice_minutes": row.voice},
			"sourceRef": "novanet/facts/plans.xlsx#Plans",
			"validFrom": row.validFrom, "validTo": nil,
		})
	}
}
