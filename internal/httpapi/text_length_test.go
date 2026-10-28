// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #367: a limit the contract states with maxLength or minLength is counted the
// way JSON Schema counts it, in characters (Unicode code points), never in
// bytes. A Chinese character is three bytes of UTF-8 and an emoji four; each
// is one character.

const (
	hanChar   = "话" // U+8BDD, three bytes of UTF-8
	emojiChar = "📞" // U+1F4DE, four bytes of UTF-8
)

// chars is n code points of pattern, repeated and cut to length.
func chars(pattern string, n int) string {
	p := []rune(pattern)
	out := make([]rune, n)
	for i := range out {
		out[i] = p[i%len(p)]
	}
	return string(out)
}

// The fixtures are what the cases say they are: more bytes than characters.
// Otherwise every case below would pass under a byte count too.
func TestTheLengthFixturesAreWiderInBytesThanInCharacters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		s         string
		wantBytes int
	}{
		{chars(hanChar, 200), 600},
		{chars(emojiChar+hanChar, 200), 700},
		{chars(hanChar, 120), 360},
		{chars(emojiChar+hanChar, 8), 28},
	} {
		if got := len(tc.s); got != tc.wantBytes {
			t.Errorf("%d characters are %d bytes, want %d", len([]rune(tc.s)), got, tc.wantBytes)
		}
	}
}

// wantFieldRefused checks the refusal shape a client keys on: 422
// VALIDATION_FAILED naming the field and the rule.
func wantFieldRefused(t *testing.T, rec *httptest.ResponseRecorder, field, rule string) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if code := errorCodeOf(t, rec); code != string(CodeValidationFailed) {
		t.Errorf("code = %q, want VALIDATION_FAILED", code)
	}
	if got := errorParamOf(t, rec, "field"); got != field {
		t.Errorf("params.field = %q, want %q", got, field)
	}
	if got := errorParamOf(t, rec, "rule"); got != rule {
		t.Errorf("params.rule = %q, want %q", got, rule)
	}
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The publish note is at most 200 characters (FlowPublish.note). An author
// writing in Chinese gets the same 200 as one writing in English, which the
// editor's input already allows.
func TestAPublishNoteIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		body     string
		accepted bool
		want     string // the note stored, when accepted
	}{
		{"200 ASCII characters", jsonBody(t, map[string]string{"note": chars("a", 200)}), true, chars("a", 200)},
		{"201 ASCII characters", jsonBody(t, map[string]string{"note": chars("a", 201)}), false, ""},
		{"200 Chinese characters, 600 bytes", jsonBody(t, map[string]string{"note": chars(hanChar, 200)}), true, chars(hanChar, 200)},
		{"201 Chinese characters", jsonBody(t, map[string]string{"note": chars(hanChar, 201)}), false, ""},
		{"200 emoji and Chinese characters, 700 bytes", jsonBody(t, map[string]string{"note": chars(emojiChar+hanChar, 200)}), true, chars(emojiChar+hanChar, 200)},
		{"201 emoji and Chinese characters", jsonBody(t, map[string]string{"note": chars(emojiChar+hanChar, 201)}), false, ""},
		{"whitespace around 200 characters is trimmed before counting",
			jsonBody(t, map[string]string{"note": " \t " + chars(hanChar, 200) + "\n  "}), true, chars(hanChar, 200)},
		{"whitespace around 201 characters does not hide the extra one",
			jsonBody(t, map[string]string{"note": " \t " + chars(hanChar, 201) + "\n  "}), false, ""},
		// The 4 KB body cap is in bytes, and the longest note still fits under
		// it when a client escapes every character: an emoji is a surrogate
		// pair, twelve bytes of JSON.
		{"200 emoji written as JSON escapes", `{"note":"` + strings.Repeat(`📞`, 200) + `"}`, true, chars(emojiChar, 200)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := flowStub()
			s := &Server{flows: f}
			rec := httptest.NewRecorder()

			s.PublishFlow(rec, post(tc.body), f.summary.ID)

			if !tc.accepted {
				wantFieldRefused(t, rec, "note", "NOTE_TOO_LONG")
				if f.published {
					t.Error("a refused note was published")
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if f.publishNote != tc.want {
				t.Errorf("note stored is %d characters, want %d (%q)",
					len([]rune(f.publishNote)), len([]rune(tc.want)), f.publishNote)
			}
		})
	}
}

// A flow's name is at most 120 characters (FlowCreate.name and
// FlowDraftWrite.name), on both writes that carry it.
func TestAFlowNameIsCountedInCharacters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		flowName string
		accepted bool
		want     string // the name stored, when accepted
	}{
		{"120 ASCII characters", chars("a", 120), true, chars("a", 120)},
		{"121 ASCII characters", chars("a", 121), false, ""},
		{"120 Chinese characters, 360 bytes", chars(hanChar, 120), true, chars(hanChar, 120)},
		{"121 Chinese characters", chars(hanChar, 121), false, ""},
		{"120 emoji and Chinese characters, 420 bytes", chars(emojiChar+hanChar, 120), true, chars(emojiChar+hanChar, 120)},
		{"121 emoji and Chinese characters", chars(emojiChar+hanChar, 121), false, ""},
		{"whitespace around 120 characters is trimmed before counting", "  " + chars(hanChar, 120) + "\t", true, chars(hanChar, 120)},
		{"whitespace around 121 characters does not hide the extra one", "  " + chars(hanChar, 121) + "\t", false, ""},
	}
	writes := []struct {
		name     string
		okStatus int
		do       func(s *Server, f *stubFlows, w http.ResponseWriter, body string)
	}{
		{"create", http.StatusCreated, func(s *Server, _ *stubFlows, w http.ResponseWriter, body string) {
			s.CreateFlow(w, post(body))
		}},
		{"save the draft", http.StatusOK, func(s *Server, f *stubFlows, w http.ResponseWriter, body string) {
			s.UpdateFlowDraft(w, httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body)), f.summary.ID)
		}},
	}
	for _, write := range writes {
		for _, tc := range cases {
			t.Run(write.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				f := flowStub()
				s := &Server{flows: f}
				rec := httptest.NewRecorder()
				body := jsonBody(t, map[string]any{
					"slug": "novanet_support", "name": tc.flowName, "spec": json.RawMessage(validSpecBody),
				})

				write.do(s, f, rec, body)

				if !tc.accepted {
					wantFieldRefused(t, rec, "name", "NAME_TOO_LONG")
					if f.draft != nil {
						t.Error("a refused name was stored")
					}
					return
				}
				if rec.Code != write.okStatus {
					t.Fatalf("status = %d, want %d: %s", rec.Code, write.okStatus, rec.Body.String())
				}
				if f.createdName != tc.want {
					t.Errorf("name stored is %d characters, want %d (%q)",
						len([]rune(f.createdName)), len([]rune(tc.want)), f.createdName)
				}
			})
		}
	}
}
