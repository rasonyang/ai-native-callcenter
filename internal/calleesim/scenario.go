// SPDX-License-Identifier: Apache-2.0

// Package calleesim plays the called party of an outbound call: a SIP user
// agent server that answers as a scenario file says, so the path from a real
// SIP response through the switch to a campaign outcome can be checked
// without a person holding a phone.
//
// It is a test tool. It reuses the SIP, SDP and RTP code of internal/voice,
// records every dialog it handled, and is built from the repository like
// aicc-loadgen; it is not shipped in a release.
package calleesim

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// Uplink kinds: what the callee sends towards the caller.
const (
	UplinkSilence = "SILENCE"
	UplinkTone    = "TONE"
	UplinkWAV     = "WAV"
	// UplinkBursts is silence with bursts of a tone at set times, counted
	// from the moment media starts: a callee that says "hello" after a pause.
	UplinkBursts = "BURSTS"
)

// Scenario maps a dialled number (the user part of the Request-URI, matched
// exactly) to the behaviours of its successive attempts.
type Scenario struct {
	Numbers map[string][]Behaviour `json:"numbers"`
}

// Behaviour is how one attempt on a number is answered.
type Behaviour struct {
	// Provisionals are sent in order of AtMs, counted from the INVITE.
	Provisionals []Provisional `json:"provisionals,omitempty"`
	// FinalStatus is 200 or any 4xx, 5xx or 6xx. Zero means 200.
	FinalStatus int `json:"finalStatus,omitempty"`
	// FinalAfterMs is the delay from the INVITE to the final response.
	FinalAfterMs int `json:"finalAfterMs,omitempty"`
	// ByeAfterMs, when set, makes the callee hang up this long after the 200
	// OK. Absent means it never does.
	ByeAfterMs *int `json:"byeAfterMs,omitempty"`
	// Uplink is the audio sent to the caller from the moment media starts.
	Uplink Uplink `json:"uplink,omitempty"`
}

// Provisional is a 1xx response.
type Provisional struct {
	// Status is 180 or 183.
	Status int `json:"status"`
	AtMs   int `json:"atMs,omitempty"`
	// EarlyMedia attaches an SDP answer (183 only) and opens the media path.
	EarlyMedia bool `json:"earlyMedia,omitempty"`
}

// Uplink selects the callee's audio.
type Uplink struct {
	// Kind is SILENCE (the default), TONE, WAV or BURSTS.
	Kind string `json:"kind,omitempty"`
	// FrequencyHz of the tone (TONE and BURSTS); 440 when zero.
	FrequencyHz int `json:"frequencyHz,omitempty"`
	// Bursts are the tone's bursts for BURSTS, each timed from media start
	// (the moment the simulator opens its RTP path, recorded as
	// DialogRecord.UplinkStartedAfterInviteMs); everything between them is
	// silence.
	Bursts []Burst `json:"bursts,omitempty"`
	// File is an 8 kHz mono 16-bit PCM WAV, played once, for WAV.
	File string `json:"file,omitempty"`
}

// Burst is one burst of tone: it sounds from AtMs for DurationMs after media
// starts.
type Burst struct {
	AtMs       int `json:"atMs"`
	DurationMs int `json:"durationMs"`
}

// ParseScenario reads and validates a scenario.
func ParseScenario(data []byte) (*Scenario, error) {
	var s Scenario
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadScenario reads a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseScenario(data)
}

// Validate rejects a scenario the simulator could not play faithfully.
func (s *Scenario) Validate() error {
	for number, behaviours := range s.Numbers {
		if number == "" {
			return fmt.Errorf("scenario: empty number")
		}
		if len(behaviours) == 0 {
			return fmt.Errorf("scenario: number %s has no behaviour", number)
		}
		for i := range behaviours {
			if err := behaviours[i].validate(); err != nil {
				return fmt.Errorf("scenario: number %s attempt %d: %w", number, i, err)
			}
		}
	}
	return nil
}

func (b *Behaviour) validate() error {
	status := b.Final()
	if status != 200 && (status < 400 || status > 699) {
		return fmt.Errorf("finalStatus %d is not 200 or 4xx/5xx/6xx", status)
	}
	if b.FinalAfterMs < 0 {
		return fmt.Errorf("finalAfterMs is negative")
	}
	if b.ByeAfterMs != nil {
		if *b.ByeAfterMs < 0 {
			return fmt.Errorf("byeAfterMs is negative")
		}
		if status != 200 {
			return fmt.Errorf("byeAfterMs needs finalStatus 200")
		}
	}
	for _, p := range b.Provisionals {
		if p.Status != 180 && p.Status != 183 {
			return fmt.Errorf("provisional %d is not 180 or 183", p.Status)
		}
		if p.EarlyMedia && p.Status != 183 {
			return fmt.Errorf("earlyMedia needs status 183")
		}
		if p.AtMs < 0 || p.AtMs > b.FinalAfterMs {
			return fmt.Errorf("provisional atMs %d is outside 0..finalAfterMs", p.AtMs)
		}
	}
	switch b.Uplink.Kind {
	case "", UplinkSilence, UplinkTone:
	case UplinkWAV:
		if b.Uplink.File == "" {
			return fmt.Errorf("uplink WAV needs a file")
		}
	case UplinkBursts:
		if len(b.Uplink.Bursts) == 0 {
			return fmt.Errorf("uplink BURSTS needs at least one burst")
		}
		for i, burst := range b.Uplink.Bursts {
			if burst.AtMs < 0 || burst.DurationMs <= 0 {
				return fmt.Errorf("uplink burst %d: atMs %d must not be negative and durationMs %d must be positive", i, burst.AtMs, burst.DurationMs)
			}
		}
	default:
		return fmt.Errorf("unknown uplink kind %q", b.Uplink.Kind)
	}
	if len(b.Uplink.Bursts) > 0 && b.Uplink.Kind != UplinkBursts {
		return fmt.Errorf("uplink bursts need kind BURSTS, not %q", b.Uplink.Kind)
	}
	return nil
}

// Final is the final status, with the 200 default applied.
func (b *Behaviour) Final() int {
	if b.FinalStatus == 0 {
		return 200
	}
	return b.FinalStatus
}

// sortedProvisionals returns the provisionals in sending order.
func (b *Behaviour) sortedProvisionals() []Provisional {
	out := append([]Provisional(nil), b.Provisionals...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	return out
}

// For returns the behaviour of attempt n (zero based) on a number; attempts
// past the end use the last entry. ok is false for a number not in the
// scenario.
func (s *Scenario) For(number string, attempt int) (Behaviour, bool) {
	list := s.Numbers[number]
	if len(list) == 0 {
		return Behaviour{}, false
	}
	if attempt >= len(list) {
		attempt = len(list) - 1
	}
	return list[attempt], true
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }
