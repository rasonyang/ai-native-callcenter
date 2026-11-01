// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// silenceRMS is the level at or below which a frame counts as silence. It is
// the simulator's own threshold (internal/calleesim/audio.go): G.711 silence
// decodes to zero and a quiet line stays well under it.
const silenceRMS = 50

// keyScopes are what the smoke's API key may do: place an AI call, read the
// ledger, and provision the flow and number the call needs. Nothing else.
var keyScopes = []string{"calls:create", "calls:create:ai", "history:read:all", "config:read", "config:write"}

// SmokeConfig is everything the smoke needs to know about its environment.
type SmokeConfig struct {
	// API is the application's REST base, for example http://10.1.2.10:8080.
	API string
	// ReadyURL is the application's readiness endpoint (the ops listener).
	ReadyURL string
	// CalleeSim is the simulator's report endpoint base.
	CalleeSim string
	// ESLAddr and ESLPassword reach the switch, for the gateway check.
	ESLAddr     string
	ESLPassword string
	// ComposeConfig is the path of the resolved compose configuration (JSON).
	ComposeConfig string

	Username string
	Password string
	// FlowFile is the flow spec to publish; To is the number the simulator
	// answers. DID is the number the call goes out from; empty means a fresh
	// random one, so a second run against the same environment provisions its
	// own and never sees the first one's rows.
	FlowFile string
	DID      string
	To       string

	// OutDir receives smoke.json, calleesim-report.json and cdr.json.
	OutDir string
	// Wait bounds each wait (readiness, the call ending, the CDR).
	Wait time.Duration
}

// Summary is what a run writes to smoke.json.
type Summary struct {
	CallID string `json:"callId"`
	To     string `json:"to"`
	DID    string `json:"did"`
	Passed bool   `json:"passed"`
	// Failures lists every assertion that did not hold.
	Failures []string `json:"failures"`
	// Checks lists every assertion that was made, passed or not.
	Checks []string `json:"checks"`

	Dialog *DialogSummary `json:"dialog,omitempty"`
	CDR    *CDRSummary    `json:"cdr,omitempty"`
	// Latency is reported and never asserted.
	Latency map[string]float64 `json:"latencyMs"`
}

// DialogSummary is the simulator's record of the call, condensed.
type DialogSummary struct {
	Number                  string   `json:"number"`
	FinalStatus             int      `json:"finalStatus"`
	Codec                   string   `json:"codec"`
	EndedBy                 string   `json:"endedBy"`
	Frames                  int      `json:"downlinkFrames"`
	PeakRMS                 float64  `json:"downlinkPeakRms"`
	FirstAudioAfterAnswerMs *float64 `json:"firstAudioAfterAnswerMs,omitempty"`
}

// CDRSummary is the ledger row, condensed.
type CDRSummary struct {
	Count    int    `json:"count"`
	CallType string `json:"callType"`
	Status   string `json:"status"`
	To       string `json:"toNumber"`
	DID      string `json:"did"`
}

type collector struct {
	checks, failures []string
}

// that records one assertion.
func (c *collector) that(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.checks = append(c.checks, msg)
	if !ok {
		c.failures = append(c.failures, msg)
	}
}

// Smoke places one AI outbound call and checks the simulator and the ledger.
// It returns an error when the run is refused, cannot proceed, or an assertion
// failed; the summary is written either way once the run started.
func Smoke(ctx context.Context, cfg SmokeConfig) error {
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return err
	}
	// Refuse before anything is sent.
	composeJSON, err := os.ReadFile(cfg.ComposeConfig)
	if err != nil {
		return fmt.Errorf("read compose config: %w", err)
	}
	if err := CheckComposeConfig(composeJSON); err != nil {
		return err
	}
	if err := checkSwitch(ctx, cfg.ESLAddr, cfg.ESLPassword); err != nil {
		return err
	}

	c := &httpc{base: cfg.API, client: newClient()}
	if err := waitReady(ctx, c.client, cfg.ReadyURL, cfg.Wait); err != nil {
		return err
	}

	if cfg.DID == "" {
		cfg.DID = fmt.Sprintf("95%06d", rand.IntN(1_000_000))
	}
	sum := &Summary{To: cfg.To, DID: cfg.DID, Latency: map[string]float64{}}
	var col collector
	err = runCall(ctx, cfg, c, sum, &col)
	sum.Checks, sum.Failures = col.checks, col.failures
	if sum.Failures == nil {
		sum.Failures = []string{}
	}
	sum.Passed = err == nil && len(col.failures) == 0
	if werr := writeJSON(filepath.Join(cfg.OutDir, "smoke.json"), sum); werr != nil && err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	if !sum.Passed {
		return fmt.Errorf("%d assertion(s) failed: %s", len(sum.Failures), strings.Join(sum.Failures, "; "))
	}
	return nil
}

// checkSwitch asks the switch for its gateways and refuses a run unless they
// are exactly the bot's and the simulator's.
func checkSwitch(ctx context.Context, addr, password string) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := esl.Dial(dctx, addr, password)
	if err != nil {
		return fmt.Errorf("cannot ask the switch for its gateways: %w", err)
	}
	defer client.Close()
	status, err := client.API("sofia status")
	if err != nil {
		return fmt.Errorf("sofia status: %w", err)
	}
	return CheckGateways(status)
}

func runCall(ctx context.Context, cfg SmokeConfig, c *httpc, sum *Summary, col *collector) error {
	if err := c.signInWithKey(ctx, cfg.Username, cfg.Password, "e2e-smoke", keyScopes); err != nil {
		return err
	}
	if _, err := c.publishFlowAndDID(ctx, cfg.FlowFile, "e2e-smoke-"+cfg.DID, "e2e smoke "+cfg.DID, cfg.DID, nil); err != nil {
		return err
	}

	// What the environment holds before the call, so that "exactly one" means
	// one more than there was, and a rerun against the same environment
	// counts only its own call.
	var before api.CDRList
	if err := c.do(ctx, http.MethodGet, "/cdrs", nil, &before, http.StatusOK); err != nil {
		return fmt.Errorf("list cdrs: %w", err)
	}
	var pre calleesim.Report
	if err := c.getJSON(ctx, cfg.CalleeSim+"/report", &pre); err != nil {
		return fmt.Errorf("simulator report: %w", err)
	}

	callID := uuid.Must(uuid.NewV7())
	sum.CallID = callID.String()
	kind := api.CreateCallRequestKind("AI_OUTBOUND")
	cid := callID
	placedAt := time.Now()
	var created api.CreateCallResponse
	if err := c.do(ctx, http.MethodPost, "/calls", api.CreateCallRequest{CallID: &cid, Kind: &kind, To: cfg.To, DID: &cfg.DID}, &created, http.StatusCreated); err != nil {
		return fmt.Errorf("place call: %w", err)
	}
	col.that(created.CallID == callID && (created.IsDuplicate == nil || !*created.IsDuplicate), "the call was placed once under the id sent")

	// The simulator ends the call by itself (its scenario says when); wait for
	// the dialog to end and the ledger row to appear.
	report, err := waitDialogEnded(ctx, c, cfg, len(pre.Dialogs))
	if err != nil {
		_ = writeJSON(filepath.Join(cfg.OutDir, "calleesim-report.json"), report)
		return err
	}
	if err := writeJSON(filepath.Join(cfg.OutDir, "calleesim-report.json"), report); err != nil {
		return err
	}
	assertDialog(report, len(pre.Dialogs), cfg, placedAt, sum, col)

	cdrs, err := waitCDR(ctx, c, cfg)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(cfg.OutDir, "cdr.json"), cdrs); err != nil {
		return err
	}
	var all api.CDRList
	if err := c.do(ctx, http.MethodGet, "/cdrs", nil, &all, http.StatusOK); err != nil {
		return fmt.Errorf("list cdrs: %w", err)
	}
	col.that(all.Total == before.Total+1, "the ledger grew by exactly one row (%d -> %d)", before.Total, all.Total)
	assertCDR(cdrs, callID, cfg, sum, col)
	return nil
}

// signInWithKey signs in once as the administrator, mints a scoped key, and
// uses only the key from there on: a call is placed, and a campaign driven,
// the way a customer's integration would.
func (c *httpc) signInWithKey(ctx context.Context, username, password, keyName string, scopes []string) error {
	var user api.CurrentUser
	if err := c.do(ctx, http.MethodPost, "/auth/login", api.LoginRequest{Username: username, Password: password}, &user, http.StatusOK); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	var key api.APIKeyCreated
	if err := c.do(ctx, http.MethodPost, "/api-keys", api.APIKeyWrite{Name: keyName, Scopes: scopes}, &key, http.StatusCreated); err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	c.bearer = key.Secret
	c.client.Jar = nil // the key is the only credential from here on
	return nil
}

// publishFlowAndDID publishes the flow in flowFile under slug and name, and
// creates an enabled DID that allows outbound calls and runs it, with
// maxOutbound as its outbound line limit (nil: none).
func (c *httpc) publishFlowAndDID(ctx context.Context, flowFile, slug, name, number string, maxOutbound *int) (api.DID, error) {
	specBytes, err := os.ReadFile(flowFile)
	if err != nil {
		return api.DID{}, err
	}
	var spec api.FlowSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return api.DID{}, fmt.Errorf("flow file: %w", err)
	}
	var flow api.Flow
	if err := c.do(ctx, http.MethodPost, "/flows", api.FlowCreate{Slug: slug, Name: name, Spec: spec}, &flow, http.StatusCreated); err != nil {
		return api.DID{}, fmt.Errorf("create flow: %w", err)
	}
	if err := c.do(ctx, http.MethodPost, "/flows/"+flow.FlowID.String()+"/publish", api.FlowPublish{}, nil, http.StatusOK, http.StatusCreated, http.StatusNoContent); err != nil {
		return api.DID{}, fmt.Errorf("publish flow: %w", err)
	}
	yes := true
	no := false
	flowID := flow.FlowID
	var did api.DID
	if err := c.do(ctx, http.MethodPost, "/dids", api.DIDWrite{
		Number: number, FlowID: &flowID, AllowOutbound: &yes, AllowInbound: &no, IsEnabled: &yes,
		MaxOutboundChannels: maxOutbound,
	}, &did, http.StatusCreated); err != nil {
		return api.DID{}, fmt.Errorf("create did: %w", err)
	}
	return did, nil
}

func assertDialog(r calleesim.Report, priorDialogs int, cfg SmokeConfig, placedAt time.Time, sum *Summary, col *collector) {
	col.that(len(r.Dialogs) == priorDialogs+1, "the simulator recorded exactly one new dialog (%d -> %d)", priorDialogs, len(r.Dialogs))
	col.that(r.UnexpectedCount == 0 && !r.Failed, "no unexpected dialog (%d)", r.UnexpectedCount)
	if len(r.Dialogs) != priorDialogs+1 {
		return
	}
	d := r.Dialogs[priorDialogs]
	col.that(d.Number == cfg.To, "the dialled number is %q (got %q)", cfg.To, d.Number)
	col.that(d.IsExpected, "the dialog is one the scenario expects")
	col.that(d.Final != nil && d.Final.Status == 200, "the call was answered with 200")
	col.that(d.AckAt != nil, "the answer was acknowledged")
	col.that(d.End != nil, "the dialog ended")
	col.that(d.Downlink.Frames > 0 && len(d.Downlink.Energy) > 0, "downlink audio arrived (%d frames in %d windows)", d.Downlink.Frames, len(d.Downlink.Energy))
	var peak float64
	for _, b := range d.Downlink.Energy {
		if b.PeakRMS > peak {
			peak = b.PeakRMS
		}
	}
	col.that(peak > silenceRMS, "the downlink is above silence (peak rms %.0f, silence %d)", peak, silenceRMS)

	ds := &DialogSummary{Number: d.Number, Codec: d.Codec, Frames: d.Downlink.Frames, PeakRMS: peak, FirstAudioAfterAnswerMs: d.Downlink.FirstAudioAfterAnswerMs}
	if d.Final != nil {
		ds.FinalStatus = d.Final.Status
		sum.Latency["placeToInvite"] = float64(d.InviteAt.Sub(placedAt).Milliseconds())
		sum.Latency["inviteToAnswer"] = float64(d.Final.SentAt.Sub(d.InviteAt).Milliseconds())
	}
	if d.End != nil {
		ds.EndedBy = d.End.By + " " + d.End.Method
	}
	if d.Downlink.FirstAudioAfterAnswerMs != nil {
		sum.Latency["firstAudioAfterAnswer"] = *d.Downlink.FirstAudioAfterAnswerMs
	}
	sum.Dialog = ds
}

func assertCDR(l api.CDRList, callID uuid.UUID, cfg SmokeConfig, sum *Summary, col *collector) {
	col.that(l.Total == 1 && len(l.Items) == 1, "exactly one CDR exists for the DID (total %d, items %d)", l.Total, len(l.Items))
	if len(l.Items) != 1 {
		return
	}
	cdr := l.Items[0]
	col.that(cdr.CallID == callID, "the CDR is the call placed (%s)", cdr.CallID)
	col.that(cdr.CallType == api.CallType("OUTBOUND"), "the CDR call type is OUTBOUND (%s)", cdr.CallType)
	col.that(cdr.ToNumber == cfg.To, "the CDR names the dialled number (%s)", cdr.ToNumber)
	gotDID := ""
	if cdr.DID != nil {
		gotDID = *cdr.DID
	}
	col.that(gotDID == cfg.DID, "the CDR names the DID (%s)", gotDID)
	col.that(cdr.AnsweredAt != nil, "the CDR records an answer")
	sum.CDR = &CDRSummary{Count: len(l.Items), CallType: string(cdr.CallType), Status: string(cdr.Status), To: cdr.ToNumber, DID: gotDID}
}

// waitDialogEnded waits for a dialog after the first prior ones, and for
// every dialog after them to have ended. The earlier dialogs are not looked
// at: a dialog refused with a final error response never records an end, so
// a run after a campaign run would otherwise wait for dialogs that cannot end.
func waitDialogEnded(ctx context.Context, c *httpc, cfg SmokeConfig, prior int) (calleesim.Report, error) {
	var r calleesim.Report
	deadline := time.Now().Add(cfg.Wait)
	for {
		if err := c.getJSON(ctx, cfg.CalleeSim+"/report", &r); err == nil && len(r.Dialogs) > prior {
			ended := true
			for _, d := range r.Dialogs[prior:] {
				if d.End == nil {
					ended = false
				}
			}
			if ended {
				return r, nil
			}
		}
		if time.Now().After(deadline) {
			return r, fmt.Errorf("the simulator did not record a new dialog that ended within %s (has %d dialog(s), %d before the call)", cfg.Wait, len(r.Dialogs), prior)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return r, err
		}
	}
}

func waitCDR(ctx context.Context, c *httpc, cfg SmokeConfig) (api.CDRList, error) {
	var l api.CDRList
	path := "/cdrs?did=" + cfg.DID
	deadline := time.Now().Add(cfg.Wait)
	for {
		if err := c.do(ctx, http.MethodGet, path, nil, &l, http.StatusOK); err == nil && len(l.Items) > 0 {
			// Let a second row, if the platform wrongly wrote one, land before
			// the cardinality check looks.
			if err := sleep(ctx, 2*time.Second); err != nil {
				return l, err
			}
			if err := c.do(ctx, http.MethodGet, path, nil, &l, http.StatusOK); err != nil {
				return l, err
			}
			return l, nil
		}
		if time.Now().After(deadline) {
			return l, fmt.Errorf("no CDR within %s", cfg.Wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return l, err
		}
	}
}

func waitReady(ctx context.Context, client *http.Client, url string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var last string
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready within %s: %s", url, wait, last)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// httpc is a small JSON client for the public API.
type httpc struct {
	base   string
	client *http.Client
	bearer string
}

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: 20 * time.Second, Jar: jar}
}

func (c *httpc) do(ctx context.Context, method, path string, in, out any, okStatus ...int) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	} else {
		req.Header.Set("X-AICC-Csrf", "1")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	for _, s := range okStatus {
		if resp.StatusCode == s {
			if out != nil && len(data) > 0 {
				if err := json.Unmarshal(data, out); err != nil {
					return fmt.Errorf("decode %s %s: %w", method, path, err)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
}

func (c *httpc) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
