// SPDX-License-Identifier: Apache-2.0

// Package phonenum holds the one definition of a canonical telephone number.
//
// Every number the dialer, the do-not-call list and the contact importer
// compare is canonical E.164: a "+" and 8 to 15 digits, the first not zero
// (design draft for campaigns, section 9.1). Anything typed by a person goes
// through Canonical first, so "+8613800138000" and "138 0013 8000" are one
// number once the default country code is 86.
//
// The dial path (internal/outbound) does not canonicalise: a single call may
// still carry a national number and is dialled as sent, with at most one
// leading "+", and the CDR stores the number as dialled. Canonical is for the
// places that compare numbers.
package phonenum

import (
	"errors"
	"strings"
)

// Reason says why a number was refused. The values are the per-row error
// codes of an import and are byte-identical in JSON.
type Reason string

const (
	// NumberEmpty means nothing was left once separators were removed.
	NumberEmpty Reason = "NUMBER_EMPTY"
	// NumberInvalid means what was left is not a canonical E.164 number.
	NumberInvalid Reason = "NUMBER_INVALID"
)

// Error is the typed refusal Canonical returns.
type Error struct {
	Reason Reason
}

func (e *Error) Error() string { return "phonenum: " + strings.ToLower(string(e.Reason)) }

// ReasonOf returns the reason carried by err, or "" when err is not an *Error.
func ReasonOf(err error) Reason {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Reason
	}
	return ""
}

const (
	minTail = 7  // digits after the leading non-zero digit
	maxTail = 14 // so 8 to 15 digits in all
)

// Canonical turns what a person typed into canonical E.164.
//
//  1. spaces, dashes, dots and round brackets are removed;
//  2. a leading "+" is kept, and a leading "00" becomes "+";
//  3. otherwise one leading trunk "0" is dropped and "+" plus
//     defaultCountryCode is prefixed;
//  4. the result must be "+", a digit 1-9, then 7 to 14 more digits.
//
// defaultCountryCode is digits only, without "+" (for example "86"). The
// application always passes one (AICC_DEFAULT_COUNTRY_CODE defaults to 1); a
// caller that passes it empty gets a national number refused.
func Canonical(raw, defaultCountryCode string) (string, error) {
	var b strings.Builder
	for _, r := range raw {
		switch r {
		case ' ', '-', '.', '(', ')':
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if s == "" {
		return "", &Error{NumberEmpty}
	}

	switch {
	case strings.HasPrefix(s, "+"):
	case strings.HasPrefix(s, "00"):
		s = "+" + s[2:]
	default:
		if defaultCountryCode == "" {
			return "", &Error{NumberInvalid}
		}
		s = "+" + defaultCountryCode + strings.TrimPrefix(s, "0")
	}
	if !IsCanonical(s) {
		return "", &Error{NumberInvalid}
	}
	return s, nil
}

// IsCanonical reports whether s is already canonical E.164.
func IsCanonical(s string) bool {
	if len(s) < 2+minTail || len(s) > 2+maxTail || s[0] != '+' {
		return false
	}
	if s[1] < '1' || s[1] > '9' {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// DialForm is how a trunk wants a number written on the wire
// (AICC_OUTBOUND_DIAL_FORM). The campaign store and every comparison stay
// canonical E.164; only the string handed to the switch changes.
type DialForm string

const (
	// FormE164 dials "+<cc><number>": the canonical form, unchanged.
	FormE164 DialForm = "E164"
	// FormE164NoPlus dials "<cc><number>".
	FormE164NoPlus DialForm = "E164_NO_PLUS"
	// FormNational dials the number without "+<cc>". A trunk prefix a
	// national number may need (the 0 of a Chinese landline area code, which
	// the canonical form drops) is not restored: that is a known limit.
	FormNational DialForm = "NATIONAL"
)

// ParseDialForm validates a setting; empty means FormE164.
func ParseDialForm(s string) (DialForm, error) {
	switch DialForm(s) {
	case "":
		return FormE164, nil
	case FormE164, FormE164NoPlus, FormNational:
		return DialForm(s), nil
	}
	return "", errors.New("AICC_OUTBOUND_DIAL_FORM must be E164, E164_NO_PLUS or NATIONAL, got " + `"` + s + `"`)
}

// DialString writes a canonical E.164 number in the form a trunk expects.
// countryCode is digits only ("86"). The second result says whether the form
// was applied: it is false, and the number is returned as E.164, when the
// input is not canonical, or for FormNational when countryCode is empty or
// the number does not start with it. An unknown form behaves as FormE164.
func DialString(e164 string, form DialForm, countryCode string) (string, bool) {
	if !isCanonical(e164) {
		return e164, false
	}
	switch form {
	case FormE164NoPlus:
		return e164[1:], true
	case FormNational:
		prefix := "+" + countryCode
		if countryCode == "" || !strings.HasPrefix(e164, prefix) || len(e164) == len(prefix) {
			return e164, false
		}
		return e164[len(prefix):], true
	}
	return e164, true
}

// isCanonical reports whether s is "+", a digit 1-9, then 7 to 14 digits.
func isCanonical(s string) bool {
	if len(s) < 1+1+minTail || len(s) > 1+1+maxTail || s[0] != '+' || s[1] == '0' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Callback turns the number a caller gave to be called back on into the form
// this deployment dials, and reports false when what was given is not a
// plausible telephone number. raw is what the speech model wrote down, so it
// is checked strictly: a callback queue row nobody can dial is worse than the
// caller's own number.
//
//  1. Full-width digits and "＋" become ASCII; spaces (ASCII and full-width),
//     "-", ".", round brackets (ASCII and full-width), "—" and "－" are
//     removed. Anything else left (letters, CJK, "转", "#", "*", a "+" not in
//     front) means this is not a number. Digits are never picked out of prose:
//     "号码是13800138000谢谢" is refused, because words around a number may be
//     a correction the model did not apply.
//  2. A leading "+" or "00" makes the rest an international number. Without
//     either, digits that start with countryCode are international when what
//     follows the code is a plausible number of that country
//     ("8613800138000"). Anything else is national.
//  3. A number of countryCode must be plausible for it. For "86": a mobile
//     number (1, then 3 to 9, then 9 digits) or a landline, said with its
//     trunk 0 when national ("01061234567") and without it after the country
//     code; a landline's area code is 10 (Beijing, the only one beginning
//     with 1) or begins with 2 to 9, and the number after the trunk 0 is 9 to
//     11 digits. For any other country code, or none, a national number is 7
//     to 15 digits. An international number of another country must be
//     canonical E.164 (IsCanonical).
//  4. The result follows the deployment's dial form (AICC_OUTBOUND_DIAL_FORM)
//     and default country code. A domestic number under FormNational, or with
//     no country code to complete it, is its national digits with the trunk 0
//     a Chinese landline needs ("01061234567", restored when the number was
//     given after "+86"); DialString's national form drops that 0, which is
//     why it is not used here. Under FormE164 and FormE164NoPlus a domestic
//     number is completed to E.164 (the trunk 0 dropped, as Canonical does)
//     and written by DialString. A foreign number is written by DialString,
//     so under FormNational it stays E.164.
//
// countryCode is digits only, without "+" ("86"); an unknown form behaves as
// FormE164.
func Callback(raw, countryCode string, form DialForm) (string, bool) {
	s, ok := callbackCharacters(raw)
	if !ok {
		return "", false
	}

	var international string
	switch {
	case strings.HasPrefix(s, "+"):
		international = s[1:]
	case strings.HasPrefix(s, "00"):
		international = s[2:]
	case countryCode != "" && strings.HasPrefix(s, countryCode):
		if national, nsn, ok := domesticNumber(s[len(countryCode):], countryCode, true); ok {
			return domesticCallback(national, nsn, countryCode, form)
		}
		fallthrough
	default:
		national, nsn, ok := domesticNumber(s, countryCode, false)
		if !ok {
			return "", false
		}
		return domesticCallback(national, nsn, countryCode, form)
	}

	if countryCode != "" && strings.HasPrefix(international, countryCode) {
		national, nsn, ok := domesticNumber(international[len(countryCode):], countryCode, true)
		if !ok {
			return "", false
		}
		return domesticCallback(national, nsn, countryCode, form)
	}
	e164 := "+" + international
	if !IsCanonical(e164) {
		return "", false
	}
	dialed, _ := DialString(e164, form, countryCode)
	return dialed, true
}

// callbackCharacters maps full-width digits and plus to ASCII and removes the
// separators people and transcripts put inside a number. It reports false
// when anything other than digits and one leading "+" is left, or nothing.
func callbackCharacters(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '０' && r <= '９':
			r = '0' + (r - '０')
		case r == '＋':
			r = '+'
		}
		switch r {
		case ' ', '　', '-', '.', '(', ')', '（', '）', '—', '－':
			continue
		}
		switch {
		case r >= '0' && r <= '9':
		case r == '+' && b.Len() == 0:
		default:
			return "", false
		}
		b.WriteRune(r)
	}
	s := b.String()
	if s == "" || s == "+" {
		return "", false
	}
	return s, true
}

// domesticNumber checks digits as a number of countryCode. isAfterCode says
// they followed the country code, where a Chinese landline has no trunk 0. It
// returns the number as dialled nationally and its national significant
// number (the part after "+<cc>" in E.164).
func domesticNumber(digits, countryCode string, isAfterCode bool) (national, nsn string, ok bool) {
	if !isAllDigits(digits) {
		return "", "", false
	}
	if countryCode != "86" {
		if len(digits) < 7 || len(digits) > 15 {
			return "", "", false
		}
		if isAfterCode {
			return digits, digits, true
		}
		return digits, strings.TrimPrefix(digits, "0"), true
	}
	switch {
	case isChineseMobile(digits):
		return digits, digits, true
	case !isAfterCode && strings.HasPrefix(digits, "0") && isChineseLandline(digits[1:]):
		return digits, digits[1:], true
	case isAfterCode && isChineseLandline(digits):
		return "0" + digits, digits, true
	}
	return "", "", false
}

// domesticCallback writes a domestic number in the deployment's dial form.
func domesticCallback(national, nsn, countryCode string, form DialForm) (string, bool) {
	if form == FormNational || countryCode == "" {
		return national, true
	}
	e164 := "+" + countryCode + nsn
	if !IsCanonical(e164) {
		return "", false
	}
	dialed, _ := DialString(e164, form, countryCode)
	return dialed, true
}

// isChineseMobile reports whether s is 1, then 3 to 9, then 9 digits.
func isChineseMobile(s string) bool {
	return len(s) == 11 && s[0] == '1' && s[1] >= '3' && s[1] <= '9'
}

// isChineseLandline reports whether s, a landline without its trunk 0, has an
// area code that exists (10, or one beginning with 2 to 9) and 9 to 11 digits.
func isChineseLandline(s string) bool {
	if len(s) < 9 || len(s) > 11 {
		return false
	}
	return (s[0] >= '2' && s[0] <= '9') || (s[0] == '1' && s[1] == '0')
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
