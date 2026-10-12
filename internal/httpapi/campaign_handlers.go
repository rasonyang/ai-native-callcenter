// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
)

// CampaignService is the campaign use cases as the API uses them
// (*campaign.Service). The handlers translate; every rule is the service's.
type CampaignService interface {
	Create(ctx context.Context, in campaign.New) (campaign.Campaign, error)
	Get(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	List(ctx context.Context, f campaign.ListFilter) ([]campaign.Campaign, int64, error)
	Update(ctx context.Context, id uuid.UUID, p campaign.Patch) (campaign.Campaign, error)
	Delete(ctx context.Context, id uuid.UUID) error
	Start(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Pause(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Resume(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	Cancel(ctx context.Context, id uuid.UUID) (campaign.Campaign, error)
	ImportContacts(ctx context.Context, id uuid.UUID, rows []campaign.ImportRow) (campaign.ImportResult, error)
	ListContacts(ctx context.Context, id uuid.UUID, f campaign.ContactFilter) ([]campaign.Contact, int64, error)
}

// maxCampaignBody bounds a campaign request body. The 5000 rows of a full
// import, each with slots at their caps, fit; the 64 KiB of the other writes
// would not.
const maxCampaignBody = 16 << 20

// ListCampaigns pages campaigns, newest first.
func (s *Server) ListCampaigns(w http.ResponseWriter, r *http.Request, params api.ListCampaignsParams) {
	items, total, err := s.campaigns.List(r.Context(), campaign.ListFilter{
		Status: campaign.Status(stringOr(params.Status)),
		Limit:  intOr(params.Limit, 50),
		Offset: intOr(params.Offset, 0),
	})
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	out := make([]api.Campaign, 0, len(items))
	for _, c := range items {
		out = append(out, campaignOut(c))
	}
	writeJSON(w, http.StatusOK, api.CampaignList{Items: out, Total: int(total)})
}

// CreateCampaign stores a DRAFT campaign.
func (s *Server) CreateCampaign(w http.ResponseWriter, r *http.Request) {
	var in api.CampaignWrite
	if _, ok := decodeCampaignBody(w, r, &in); !ok {
		return
	}
	n := campaign.New{
		Name: in.Name, DIDID: in.DIDID, TimeZone: in.TimeZone,
		CallingWindow:      callingWindowIn(in.CallingWindow),
		DefaultCountryCode: deref(in.DefaultCountryCode),
		MaxConcurrent:      in.MaxConcurrent,
		RingTimeoutSec:     in.RingTimeoutSec,
		FollowUpQueueID:    in.FollowUpQueueID,
	}
	if in.RetryPolicy != nil {
		p := retryPolicyIn(*in.RetryPolicy)
		n.RetryPolicy = &p
	}
	c, err := s.campaigns.Create(r.Context(), n)
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, campaignOut(c))
}

// GetCampaign returns one campaign.
func (s *Server) GetCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	c, err := s.campaigns.Get(r.Context(), campaignID)
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, campaignOut(c))
}

// UpdateCampaign changes a campaign. Absent fields are left alone and null
// clears the two optional ones; the body is read twice, once for which fields
// it names and once for their values, because a pointer cannot tell an absent
// field from a null.
func (s *Server) UpdateCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	var in api.CampaignPatch
	present, ok := decodeCampaignBody(w, r, &in)
	if !ok {
		return
	}
	patch := campaign.Patch{
		Name: in.Name, DIDID: in.DIDID, TimeZone: in.TimeZone,
		MaxConcurrent: in.MaxConcurrent, RingTimeoutSec: in.RingTimeoutSec,
	}
	if in.CallingWindow != nil {
		cw := callingWindowIn(*in.CallingWindow)
		patch.CallingWindow = &cw
	}
	if in.RetryPolicy != nil {
		rp := retryPolicyIn(*in.RetryPolicy)
		patch.RetryPolicy = &rp
	}
	if isNull(present["defaultCountryCode"]) {
		patch.ClearDefaultCountryCode = true
	} else {
		patch.DefaultCountryCode = in.DefaultCountryCode
	}
	if isNull(present["followUpQueueId"]) {
		patch.ClearFollowUpQueue = true
	} else {
		patch.FollowUpQueueID = in.FollowUpQueueID
	}
	// A field named with null that is not one of the two clearable ones is not
	// a request to clear it.
	for _, name := range []string{"name", "didId", "timeZone", "callingWindow", "maxConcurrent", "ringTimeoutSec", "retryPolicy"} {
		if isNull(present[name]) {
			writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
				name+" cannot be null; leave it out to keep it", map[string]any{"field": name, "rule": "NOT_NULLABLE"})
			return
		}
	}
	c, err := s.campaigns.Update(r.Context(), campaignID, patch)
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, campaignOut(c))
}

// DeleteCampaign removes a DRAFT campaign.
func (s *Server) DeleteCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	if err := s.campaigns.Delete(r.Context(), campaignID); err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StartCampaign moves DRAFT to RUNNING.
func (s *Server) StartCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	s.campaignMove(w, r, s.campaigns.Start, campaignID)
}

// PauseCampaign moves RUNNING to PAUSED.
func (s *Server) PauseCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	s.campaignMove(w, r, s.campaigns.Pause, campaignID)
}

// ResumeCampaign moves PAUSED to RUNNING.
func (s *Server) ResumeCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	s.campaignMove(w, r, s.campaigns.Resume, campaignID)
}

// CancelCampaign moves DRAFT, RUNNING or PAUSED to CANCELLED.
func (s *Server) CancelCampaign(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	s.campaignMove(w, r, s.campaigns.Cancel, campaignID)
}

func (s *Server) campaignMove(w http.ResponseWriter, r *http.Request,
	move func(context.Context, uuid.UUID) (campaign.Campaign, error), id uuid.UUID) {
	c, err := move(r.Context(), id)
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, campaignOut(c))
}

// ListCampaignContacts pages a campaign's contacts.
func (s *Server) ListCampaignContacts(w http.ResponseWriter, r *http.Request,
	campaignID openapi_types.UUID, params api.ListCampaignContactsParams) {
	items, total, err := s.campaigns.ListContacts(r.Context(), campaignID, campaign.ContactFilter{
		Status:     campaign.ContactStatus(stringOr(params.Status)),
		LeadIntent: campaign.LeadIntent(stringOr(params.LeadIntent)),
		Limit:      intOr(params.Limit, 50),
		Offset:     intOr(params.Offset, 0),
	})
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	out := make([]api.CampaignContact, 0, len(items))
	for _, c := range items {
		out = append(out, campaignContactOut(c))
	}
	writeJSON(w, http.StatusOK, api.CampaignContactList{Items: out, Total: int(total)})
}

// ImportCampaignContacts adds a list of contacts and answers one result per
// row.
func (s *Server) ImportCampaignContacts(w http.ResponseWriter, r *http.Request, campaignID openapi_types.UUID) {
	var in api.CampaignContactsImport
	if _, ok := decodeCampaignBody(w, r, &in); !ok {
		return
	}
	rows := make([]campaign.ImportRow, len(in.Contacts))
	for i, c := range in.Contacts {
		rows[i] = campaign.ImportRow{Number: c.Number, TimeZone: deref(c.TimeZone)}
		if c.Slots != nil {
			rows[i].Slots = *c.Slots
		}
	}
	res, err := s.campaigns.ImportContacts(r.Context(), campaignID, rows)
	if err != nil {
		s.writeCampaignError(w, r, err)
		return
	}
	out := api.CampaignImportResult{
		Imported: res.Imported, Duplicates: res.Duplicates, Rejected: res.Rejected,
		Rows: make([]api.CampaignImportRowResult, len(res.Rows)),
	}
	for i, row := range res.Rows {
		item := api.CampaignImportRowResult{Index: row.Index, Status: api.CampaignImportRowStatus(row.Status)}
		if row.ContactID != uuid.Nil {
			id := row.ContactID
			item.ContactID = &id
		}
		if row.Number != "" {
			n := row.Number
			item.Number = &n
		}
		if row.Code != "" {
			code := api.CampaignImportRowCode(row.Code)
			item.Code = &code
		}
		if row.Detail != "" {
			d := row.Detail
			item.Detail = &d
		}
		out.Rows[i] = item
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeCampaignBody reads a JSON body of at most maxCampaignBody bytes into v,
// refusing a property the contract does not declare (a misspelt field would
// otherwise be ignored and the client told its change was made). It returns
// the names of the top-level properties present.
func decodeCampaignBody(w http.ResponseWriter, r *http.Request, v any) (map[string]json.RawMessage, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCampaignBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeValidationFailed,
				"the request body is larger than 16 MiB; send the list in several requests",
				map[string]any{"rule": "BODY_TOO_LARGE", "maxBytes": maxCampaignBody})
			return nil, false
		}
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body", nil)
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "malformed request body: "+err.Error(), nil)
		return nil, false
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "the request body must be a JSON object", nil)
		return nil, false
	}
	return present, true
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// writeCampaignError maps the campaign package's refusals onto the contract's
// codes.
func (s *Server) writeCampaignError(w http.ResponseWriter, r *http.Request, err error) {
	if status, code, message, params, ok := campaignRefusal(err); ok {
		writeError(w, status, code, message, params)
		return
	}
	slog.ErrorContext(r.Context(), "campaign request failed", "error", err)
	writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot complete the request", nil)
}

// campaignRefusal reads a campaign refusal out of err. It is shared with the
// handlers whose operations the campaign rules can also refuse (publishing a
// flow, rebinding a DID).
func campaignRefusal(err error) (status int, code ErrorCode, message string, params map[string]any, ok bool) {
	switch {
	case errors.Is(err, campaign.ErrDIDNotFound):
		return http.StatusUnprocessableEntity, CodeValidationFailed, "no such DID",
			map[string]any{"field": "didId", "rule": "DID_NOT_FOUND"}, true
	case errors.Is(err, campaign.ErrQueueNotFound):
		return http.StatusUnprocessableEntity, CodeValidationFailed, "no such queue",
			map[string]any{"field": "followUpQueueId", "rule": "QUEUE_NOT_FOUND"}, true
	}
	var ce *campaign.Error
	if !errors.As(err, &ce) {
		return 0, "", "", nil, false
	}
	params = map[string]any{}
	for k, v := range ce.Params {
		params[k] = v
	}
	if ce.Field != "" {
		params["field"] = ce.Field
	}
	if ce.Rule != "" && ce.Kind != campaign.ErrInvalidTransition {
		params["rule"] = ce.Rule
	}
	if len(params) == 0 {
		params = nil
	}
	switch {
	case errors.Is(ce.Kind, campaign.ErrNotFound):
		return http.StatusNotFound, CodeNotFound, "no such campaign", nil, true
	case errors.Is(ce.Kind, campaign.ErrValidation):
		return http.StatusUnprocessableEntity, CodeValidationFailed, ce.Message, params, true
	case errors.Is(ce.Kind, campaign.ErrInvalidTransition):
		return http.StatusConflict, CodeCampaignInvalidTransition, ce.Message, params, true
	case errors.Is(ce.Kind, campaign.ErrFieldLocked):
		return http.StatusConflict, CodeCampaignFieldLocked, ce.Message, params, true
	case errors.Is(ce.Kind, campaign.ErrRingTimeoutTooLong):
		return http.StatusUnprocessableEntity, CodeCampaignRingTimeoutTooLong, ce.Message, params, true
	case errors.Is(ce.Kind, campaign.ErrWindowOutsideLimit):
		return http.StatusUnprocessableEntity, CodeCampaignWindowOutsideLimit, ce.Message, params, true
	case errors.Is(ce.Kind, campaign.ErrFlowHasTransfer):
		// A campaign being written is refused for what it asks (422). A flow
		// publish or a DID rebinding is refused for the state it would break
		// (409): the request is fine, a campaign stands in its way.
		if ce.Field == "" {
			return http.StatusConflict, CodeCampaignFlowHasTransfer, ce.Message, params, true
		}
		return http.StatusUnprocessableEntity, CodeCampaignFlowHasTransfer, ce.Message, params, true
	}
	return 0, "", "", nil, false
}

func callingWindowIn(w api.CampaignCallingWindow) campaign.CallingWindow {
	out := campaign.CallingWindow{
		Days:   make([]string, len(w.Days)),
		Ranges: make([]campaign.TimeRange, len(w.Ranges)),
	}
	for i, d := range w.Days {
		out.Days[i] = string(d)
	}
	for i, r := range w.Ranges {
		out.Ranges[i] = campaign.TimeRange{Start: r.Start, End: r.End}
	}
	return out
}

// retryPolicyIn reads a policy as written: a field left out takes its default,
// never the stored value.
func retryPolicyIn(w api.CampaignRetryPolicyWrite) campaign.RetryPolicy {
	p := campaign.DefaultRetryPolicy()
	if w.MaxAttempts != nil {
		p.MaxAttempts = *w.MaxAttempts
	}
	if w.MaxSystemRetries != nil {
		p.MaxSystemRetries = *w.MaxSystemRetries
	}
	if w.RetryOutcomes != nil {
		p.RetryOutcomes = make([]campaign.Outcome, len(*w.RetryOutcomes))
		for i, o := range *w.RetryOutcomes {
			p.RetryOutcomes[i] = campaign.Outcome(o)
		}
	}
	if w.IntervalSec != nil {
		// What the client names is validated, not silently dropped: an
		// outcome with no interval is the policy's, and one that cannot have
		// an interval is an error Normalize reports.
		for o, sec := range *w.IntervalSec {
			p.IntervalSec[campaign.Outcome(o)] = sec
		}
	}
	if w.TransientBackoff != nil {
		if w.TransientBackoff.BaseSec != nil {
			p.TransientBackoff.BaseSec = *w.TransientBackoff.BaseSec
		}
		if w.TransientBackoff.Factor != nil {
			p.TransientBackoff.Factor = *w.TransientBackoff.Factor
		}
		if w.TransientBackoff.CapSec != nil {
			p.TransientBackoff.CapSec = *w.TransientBackoff.CapSec
		}
	}
	return p
}

func campaignOut(c campaign.Campaign) api.Campaign {
	days := make([]api.Weekday, len(c.CallingWindow.Days))
	for i, d := range c.CallingWindow.Days {
		days[i] = api.Weekday(d)
	}
	ranges := make([]api.CampaignTimeRange, len(c.CallingWindow.Ranges))
	for i, r := range c.CallingWindow.Ranges {
		ranges[i] = api.CampaignTimeRange{Start: r.Start, End: r.End}
	}
	outcomes := make([]api.CampaignRetryOutcome, len(c.RetryPolicy.RetryOutcomes))
	for i, o := range c.RetryPolicy.RetryOutcomes {
		outcomes[i] = api.CampaignRetryOutcome(o)
	}
	intervals := make(api.CampaignIntervalSec, len(c.RetryPolicy.IntervalSec))
	for o, sec := range c.RetryPolicy.IntervalSec {
		intervals[string(o)] = sec
	}
	out := api.Campaign{
		ID: c.ID, Name: c.Name, DIDID: c.DIDID, DIDNumber: c.DIDNumber,
		Status: api.CampaignStatus(c.Status), TimeZone: c.TimeZone,
		CallingWindow: api.CampaignCallingWindow{Days: days, Ranges: ranges},
		MaxConcurrent: c.MaxConcurrent, RingTimeoutSec: c.RingTimeoutSec,
		RetryPolicy: api.CampaignRetryPolicy{
			MaxAttempts: c.RetryPolicy.MaxAttempts, MaxSystemRetries: c.RetryPolicy.MaxSystemRetries,
			RetryOutcomes: outcomes, IntervalSec: intervals,
			TransientBackoff: api.CampaignBackoff{
				BaseSec: c.RetryPolicy.TransientBackoff.BaseSec,
				Factor:  c.RetryPolicy.TransientBackoff.Factor,
				CapSec:  c.RetryPolicy.TransientBackoff.CapSec,
			},
		},
		FollowUpQueueID: c.FollowUpQueueID,
		CreatedAt:       c.CreatedAt, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt,
		Contacts: api.CampaignContactCounts{
			Total: c.Contacts.Total, Active: c.Contacts.Active, Done: c.Contacts.Done,
			Exhausted: c.Contacts.Exhausted, InvalidNumber: c.Contacts.InvalidNumber, DNC: c.Contacts.DNC,
		},
	}
	if c.PauseReason != "" {
		reason := api.CampaignPauseReason(c.PauseReason)
		out.PauseReason = &reason
	}
	if c.DefaultCountryCode != "" {
		cc := c.DefaultCountryCode
		out.DefaultCountryCode = &cc
	}
	return out
}

func campaignContactOut(c campaign.Contact) api.CampaignContact {
	out := api.CampaignContact{
		ID: c.ID, CampaignID: c.CampaignID, Number: c.Number, NumberRaw: c.NumberRaw,
		Slots: c.Slots, Status: api.CampaignContactStatus(c.Status),
		Attempts: c.Attempts, SystemFailures: c.SystemFailures,
		NextAttemptAt: c.NextAttemptAt, CreatedAt: c.CreatedAt,
	}
	if out.Slots == nil {
		out.Slots = map[string]string{}
	}
	if c.TimeZone != "" {
		tz := c.TimeZone
		out.TimeZone = &tz
	}
	if c.LastOutcome != "" {
		o := api.CampaignOutcome(c.LastOutcome)
		out.LastOutcome = &o
	}
	if c.LeadIntent != "" {
		i := api.CampaignLeadIntent(c.LeadIntent)
		out.LeadIntent = &i
	}
	if c.LeadNotes != "" {
		n := c.LeadNotes
		out.LeadNotes = &n
	}
	return out
}
