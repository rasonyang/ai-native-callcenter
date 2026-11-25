// SPDX-License-Identifier: Apache-2.0

package phonenum

import "testing"

func TestCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, cc, want string
		reason             Reason
	}{
		{"already canonical", "+8613800138000", "86", "+8613800138000", ""},
		{"national with trunk zero", "013800138000", "86", "+8613800138000", ""},
		{"national without trunk zero", "13800138000", "86", "+8613800138000", ""},
		{"spaced national", "138 0013 8000", "86", "+8613800138000", ""},
		{"spaced plus", "+86 138 0013 8000", "86", "+8613800138000", ""},
		{"us punctuation", "+1 (415) 555-0100", "86", "+14155550100", ""},
		{"dots", "+44.20.7946.0958", "86", "+442079460958", ""},
		{"double zero", "008613800138000", "1", "+8613800138000", ""},
		{"double zero with separators", "00 86-138-0013-8000", "1", "+8613800138000", ""},
		{"empty", "", "86", "", NumberEmpty},
		{"separators only", " - ", "86", "", NumberEmpty},
		{"plus alone", "+", "86", "", NumberInvalid},
		{"double plus", "++8613800138000", "86", "", NumberInvalid},
		{"plus inside", "86+13800138000", "86", "", NumberInvalid},
		{"zero after plus", "+0123456789", "86", "", NumberInvalid},
		{"too short", "+1234567", "86", "", NumberInvalid},
		{"shortest", "+12345678", "86", "+12345678", ""},
		{"longest", "+123456789012345", "86", "+123456789012345", ""},
		{"too long", "+1234567890123456", "86", "", NumberInvalid},
		{"letters", "+86138abc", "86", "", NumberInvalid},
		{"trailing newline", "+8613800138000\n", "86", "", NumberInvalid},
		{"full-width plus", "＋8613800138000", "86", "", NumberInvalid},
		{"arabic-indic digits", "+٨٦١٣٨٠٠١٣٨٠٠٠", "86", "", NumberInvalid},
		{"national needs a country code", "13800138000", "", "", NumberInvalid},
		{"an extension is too short", "1002", "86", "", NumberInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonical(tt.in, tt.cc)
			if tt.reason != "" {
				if ReasonOf(err) != tt.reason {
					t.Fatalf("Canonical(%q) = %q, %v; want reason %s", tt.in, got, err, tt.reason)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Canonical(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
			if !IsCanonical(got) {
				t.Errorf("result %q is not canonical", got)
			}
		})
	}
}

func TestReasonOfIgnoresOtherErrors(t *testing.T) {
	t.Parallel()
	if got := ReasonOf(nil); got != "" {
		t.Errorf("ReasonOf(nil) = %q", got)
	}
}

func TestDialString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in string
		form     DialForm
		cc       string
		want     string
		applied  bool
	}{
		{"e164 mobile", "+8618688886666", FormE164, "86", "+8618688886666", true},
		{"unknown form is e164", "+8618688886666", "BOGUS", "86", "+8618688886666", true},
		{"no plus mobile", "+8618688886666", FormE164NoPlus, "86", "8618688886666", true},
		{"national mobile", "+8618688886666", FormNational, "86", "18688886666", true},
		{"national landline loses no digit but gains no 0", "+862166668888", FormNational, "86", "2166668888", true},
		{"other country falls back", "+14155550123", FormNational, "86", "+14155550123", false},
		{"other country matches its own code", "+14155550123", FormNational, "1", "4155550123", true},
		{"national without a country code", "+8618688886666", FormNational, "", "+8618688886666", false},
		{"no plus other country", "+14155550123", FormE164NoPlus, "86", "14155550123", true},
		{"invalid: no plus", "8618688886666", FormNational, "86", "8618688886666", false},
		{"invalid: short", "+1234", FormE164NoPlus, "1", "+1234", false},
		{"invalid: letters", "+86186abc86666", FormNational, "86", "+86186abc86666", false},
		{"invalid: empty", "", FormNational, "86", "", false},
		{"invalid: leading zero", "+0123456789", FormNational, "86", "+0123456789", false},
	}
	for _, c := range cases {
		got, ok := DialString(c.in, c.form, c.cc)
		if got != c.want || ok != c.applied {
			t.Errorf("%s: DialString(%q,%s,%q) = %q,%v want %q,%v", c.name, c.in, c.form, c.cc, got, ok, c.want, c.applied)
		}
	}
}

func TestParseDialForm(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]DialForm{"": FormE164, "E164": FormE164, "E164_NO_PLUS": FormE164NoPlus, "NATIONAL": FormNational} {
		if got, err := ParseDialForm(in); err != nil || got != want {
			t.Errorf("ParseDialForm(%q) = %q,%v", in, got, err)
		}
	}
	for _, in := range []string{"national", "E.164", "PLUS"} {
		if _, err := ParseDialForm(in); err == nil {
			t.Errorf("ParseDialForm(%q) accepted", in)
		}
	}
}

// The callback numbers of issue #311, as a speech model writes them down, in
// every dial form. want is per form: E164, E164_NO_PLUS, NATIONAL; "" means
// refused.
func TestCallback(t *testing.T) {
	t.Parallel()
	const refused = ""
	cases := []struct {
		name, in, cc string
		e164, noPlus string
		national     string
	}{
		// What the model wrote instead of a number.
		{"a description of the caller's number", "当前来电号码", "86", refused, refused, refused},
		{"this phone, in Chinese", "本机号码", "86", refused, refused, refused},
		{"this number", "this number", "86", refused, refused, refused},
		{"empty", "", "86", refused, refused, refused},
		{"whitespace only", "   ", "86", refused, refused, refused},
		{"full-width whitespace only", "　　", "86", refused, refused, refused},
		{"separators only", "-()-", "86", refused, refused, refused},
		{"plus alone", "+", "86", refused, refused, refused},
		// Words around digits are refused, not mined for a number.
		{"digits inside Chinese words", "号码是13800138000谢谢", "86", refused, refused, refused},
		{"an extension after the number", "13800138000转123", "86", refused, refused, refused},
		{"a hash", "13800138000#", "86", refused, refused, refused},
		{"a star", "*13800138000", "86", refused, refused, refused},
		{"letters", "1380013800O", "86", refused, refused, refused},
		{"a plus inside", "86+13800138000", "86", refused, refused, refused},
		{"two pluses", "++8613800138000", "86", refused, refused, refused},
		{"a newline", "13800138000\n", "86", refused, refused, refused},
		// Implausible digits.
		{"one digit short", "1380013800", "86", refused, refused, refused},
		{"one digit long", "138001380001", "86", refused, refused, refused},
		{"a landline too long", "0571888866661", "86", refused, refused, refused},
		{"forty digits", "1234567890123456789012345678901234567890", "86", refused, refused, refused},
		{"forty digits without a country code", "1234567890123456789012345678901234567890", "", refused, refused, refused},
		{"not a mobile prefix", "12800138000", "86", refused, refused, refused},
		{"a landline without its trunk zero", "1061234567", "86", refused, refused, refused},
		{"an area code beginning with 1 that is not 10", "01161234567", "86", refused, refused, refused},
		{"a landline too short", "010612345", "86", refused, refused, refused},
		{"an extension", "1002", "86", refused, refused, refused},
		{"plus 86 and a short mobile", "+86 1380013800", "86", refused, refused, refused},
		{"plus 86 and a trunk zero", "+86 010 6123 4567", "86", refused, refused, refused},
		{"a foreign number too long", "+1234567890123456", "86", refused, refused, refused},
		{"a foreign number beginning with zero", "+0123456789", "86", refused, refused, refused},
		{"national too short without a country code", "123456", "", refused, refused, refused},
		// Mobiles.
		{"a mobile", "13800138000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"spaced", "138 0013 8000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"dashed", "138-0013-8000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"dotted", "138.0013.8000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"plus 86", "+86 13800138000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"plus 86 in brackets", "(+86) 138-0013-8000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"86 without a plus", "8613800138000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"double zero 86", "0086 138 0013 8000", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"full-width digits", "１３８００１３８０００", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"full-width plus and spaces", "＋８６　１３８　００１３　８０００", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		{"full-width brackets and dashes", "（１３８）－００１３—８０００", "86",
			"+8613800138000", "8613800138000", "13800138000"},
		// Landlines keep the trunk zero nationally and lose it in E.164.
		{"a Beijing landline in brackets", "(010) 6123 4567", "86",
			"+861061234567", "861061234567", "01061234567"},
		{"a Shanghai landline", "021-6666-8888", "86",
			"+862166668888", "862166668888", "02166668888"},
		{"a three-digit area code", "0571 8888 6666", "86",
			"+8657188886666", "8657188886666", "057188886666"},
		{"a landline after plus 86", "+86 10 6123 4567", "86",
			"+861061234567", "861061234567", "01061234567"},
		{"a landline after bare 86", "861061234567", "86",
			"+861061234567", "861061234567", "01061234567"},
		// Another country stays E.164 under NATIONAL.
		{"a US number", "+1 (415) 555-0100", "86",
			"+14155550100", "14155550100", "+14155550100"},
		{"a UK number after double zero", "0044 20 7946 0958", "86",
			"+442079460958", "442079460958", "+442079460958"},
		// Another deployment country: 7 to 15 national digits.
		{"a US national number", "(415) 555-0100", "1",
			"+14155550100", "14155550100", "4155550100"},
		{"a US number with its country code", "14155550100", "1",
			"+14155550100", "14155550100", "4155550100"},
		{"a UK national number keeps its trunk zero nationally", "020 7946 0958", "44",
			"+442079460958", "442079460958", "02079460958"},
		// No country code: a national number cannot be completed, so it is
		// kept as given in every form.
		{"no country code, national", "138 0013 8000", "",
			"13800138000", "13800138000", "13800138000"},
		{"no country code, international", "+86 138 0013 8000", "",
			"+8613800138000", "8613800138000", "+8613800138000"},
	}
	forms := []DialForm{FormE164, FormE164NoPlus, FormNational}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for i, want := range []string{c.e164, c.noPlus, c.national} {
				got, ok := Callback(c.in, c.cc, forms[i])
				if ok != (want != refused) || got != want {
					t.Errorf("Callback(%q, %q, %s) = %q, %v; want %q, %v",
						c.in, c.cc, forms[i], got, ok, want, want != refused)
				}
			}
		})
	}
}

// An unknown form is E.164, as it is for DialString.
func TestCallbackUnknownFormIsE164(t *testing.T) {
	t.Parallel()
	for _, form := range []DialForm{"", "BOGUS"} {
		if got, ok := Callback("13800138000", "86", form); !ok || got != "+8613800138000" {
			t.Errorf("Callback with form %q = %q, %v", form, got, ok)
		}
	}
}

func TestCanonicalPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, cc, want string
		reason             Reason
	}{
		{"national digits", "138", "86", "+86138", ""},
		{"trunk zero dropped", "010", "86", "+8610", ""},
		{"a lone zero", "0", "86", "+86", ""},
		{"plus kept", "+86138", "86", "+86138", ""},
		{"plus and spaces", "+86 138", "86", "+86138", ""},
		{"double zero", "0086138", "86", "+86138", ""},
		{"double zero and dash", "0086-138", "1", "+86138", ""},
		{"brackets", "(010)", "86", "+8610", ""},
		{"spaced national", "138 0013", "86", "+861380013", ""},
		{"another country", "+1", "86", "+1", ""},
		{"a bare one is national", "1", "86", "+861", ""},
		{"empty", "", "86", "", ""},
		{"only separators", " - ", "86", "", ""},
		{"letters", "abc", "86", "", NumberInvalid},
		{"letters after plus", "+86abc", "86", "", NumberInvalid},
		{"no default code", "138", "", "", NumberInvalid},
		{"plus needs no default code", "+86138", "", "+86138", ""},
	}
	for _, tt := range tests {
		got, err := CanonicalPrefix(tt.in, tt.cc)
		if got != tt.want || ReasonOf(err) != tt.reason {
			t.Errorf("%s: CanonicalPrefix(%q, %q) = %q, %v; want %q, %q", tt.name, tt.in, tt.cc, got, err, tt.want, tt.reason)
		}
	}
}

// The same cases web/src/lib/phonenum.test.ts holds nationalNumber to: the
// call-record export and the contact export write a number the same way.
func TestNational(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, cc, want string
	}{
		{"a mobile number of the default code", "+8613800138000", "86", "13800138000"},
		{"another mobile prefix", "+8619900000000", "86", "19900000000"},
		{"a Beijing landline gets its trunk 0 back", "+861060000000", "86", "01060000000"},
		{"a Shanghai landline gets its trunk 0 back", "+862155550101", "86", "02155550101"},
		{"a three-digit area code", "+8675560000000", "86", "075560000000"},
		{"another country stays international", "+447700900123", "86", "+447700900123"},
		{"+1 under 86 stays international", "+12025550123", "86", "+12025550123"},
		{"a +1 number under 1", "+12025550123", "1", "2025550123"},
		{"a +44 number under 44", "+447700900123", "44", "7700900123"},
		{"no country code leaves it", "+8613800138000", "", "+8613800138000"},
		{"a national number as dialled is left", "13800138000", "86", "13800138000"},
		{"a leading-zero landline as dialled is left", "02155550101", "86", "02155550101"},
		{"an extension is left", "1001", "86", "1001"},
		{"the code alone is left", "+86", "86", "+86"},
		{"digits after the code starting with 0 stay E.164", "+86000000000", "86", "+86000000000"},
		{"neither mobile nor landline is its digits", "+8610000000", "86", "10000000"},
		{"a code that is not digits leaves it", "+8613800138000", "+86", "+8613800138000"},
		{"an anonymous caller is left", "anonymous", "86", "anonymous"},
		{"empty stays empty", "", "86", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := National(tt.in, tt.cc); got != tt.want {
				t.Errorf("National(%q, %q) = %q, want %q", tt.in, tt.cc, got, tt.want)
			}
		})
	}
}
