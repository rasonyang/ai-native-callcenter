// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FreeSWITCH's built-in RTP port range, used when switch.conf sets none (or 0).
const (
	DefaultSwitchRTPStart = 16384
	DefaultSwitchRTPEnd   = 32768
)

// RTPRange is the switch's RTP range as it will be allocated.
type RTPRange struct {
	Start, End int
	// StartSet and EndSet say whether switch.conf names the value; a missing
	// one is the switch's built-in default.
	StartSet, EndSet bool
}

// EvenStart and EvenEnd are the ports the allocator really uses: only even
// ports carry RTP, an odd start is rounded up and an odd end down.
func (r RTPRange) EvenStart() int { return r.Start + r.Start%2 }
func (r RTPRange) EvenEnd() int   { return r.End - r.End%2 }

// Legs is how many media legs the range holds, one even port each.
func (r RTPRange) Legs() int {
	if r.EvenEnd() < r.EvenStart() {
		return 0
	}
	return (r.EvenEnd()-r.EvenStart())/2 + 1
}

// IsBuiltIn reports a range the switch takes from FreeSWITCH, both ends unset.
func (r RTPRange) IsBuiltIn() bool { return !r.StartSet && !r.EndSet }

// RTPValueError is a configured value that is not a usable port, as opposed to
// a reply that could not be read at all.
type RTPValueError struct{ Msg string }

func (e RTPValueError) Error() string { return e.Msg }

// IsRTPValueError reports whether err is, or wraps, an RTPValueError.
func IsRTPValueError(err error) bool { return errors.As(err, new(RTPValueError)) }

// ParseSwitchRTPRange reads the range out of the XML `xml_locate` returns for
// switch.conf. A param that is missing, empty or 0 is the built-in default, as
// the switch itself reads it.
func ParseSwitchRTPRange(conf string) (RTPRange, error) {
	var doc struct {
		XMLName xml.Name `xml:"configuration"`
		Name    string   `xml:"name,attr"`
		Params  []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"settings>param"`
	}
	if err := xml.Unmarshal([]byte(strings.TrimSpace(conf)), &doc); err != nil {
		return RTPRange{}, fmt.Errorf("the reply is not switch.conf XML: %w", err)
	}
	if doc.Name != "switch.conf" {
		return RTPRange{}, fmt.Errorf("the reply is configuration %q, not switch.conf", doc.Name)
	}
	r := RTPRange{Start: DefaultSwitchRTPStart, End: DefaultSwitchRTPEnd}
	for _, p := range doc.Params {
		var target *int
		var set *bool
		switch p.Name {
		case "rtp-start-port":
			target, set = &r.Start, &r.StartSet
		case "rtp-end-port":
			target, set = &r.End, &r.EndSet
		default:
			continue
		}
		v := strings.TrimSpace(p.Value)
		if v == "" || v == "0" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return RTPRange{}, RTPValueError{fmt.Sprintf("%s is %q, not a port between 1 and 65535", p.Name, v)}
		}
		*target, *set = n, true
	}
	return r, nil
}

// SwitchRTPRange reads the switch's configured RTP range over ESL. It is the
// configured text, not the running allocator's state (see SwitchConf).
func (a *Adapter) SwitchRTPRange() (RTPRange, error) {
	conf, err := a.SwitchConf()
	if err != nil {
		return RTPRange{}, err
	}
	return ParseSwitchRTPRange(conf)
}

// RTPRangesOverlap reports whether the bot's range [botLow, botHigh] and the
// switch's share a port. Both ends are inclusive. The switch's whole configured
// span counts, odd ports included: the bot allocates RTP/RTCP pairs and the
// switch may bind either parity of a neighbouring port.
func RTPRangesOverlap(botLow, botHigh int, sw RTPRange) bool {
	return botLow <= sw.End && sw.Start <= botHigh
}
