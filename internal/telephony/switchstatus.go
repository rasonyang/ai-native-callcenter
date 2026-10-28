// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"fmt"
	"strings"
)

// SwitchProfile is one of the switch's SIP listeners as it runs right now.
type SwitchProfile struct {
	Name string
	// IsRunning is whether the listener is up and taking traffic.
	IsRunning bool
	// AdvertisedMediaIP is the address the listener puts in its SDP for media,
	// which is what a phone or a carrier sends RTP to. Empty when the listener
	// advertises no address of its own (the IPv6 listeners on a stock
	// install), or when the switch would not describe it.
	AdvertisedMediaIP string
}

// ErrUnknownGateway reports that the switch holds no gateway by that name.
var ErrUnknownGateway = errors.New("the switch has no gateway by that name")

// Profiles asks the switch which SIP listeners it runs, whether each is
// running, and what media address each advertises.
//
// Read, never written. It costs one command for the list and one per listener
// for its detail, which is a handful on any install; it is for a diagnostic,
// not for anything on a call path.
func (a *Adapter) Profiles() ([]SwitchProfile, error) {
	out, err := a.cmd.API("sofia status")
	if err != nil {
		return nil, err
	}
	profiles := parseProfiles(out)
	for i := range profiles {
		detail, err := a.api("sofia status profile %s", profiles[i].Name)
		if err != nil {
			return nil, err
		}
		profiles[i].AdvertisedMediaIP = statusField(detail, "Ext-RTP-IP")
	}
	return profiles, nil
}

// GatewayUp asks the switch whether it considers a gateway reachable.
//
// This is the switch's own verdict from its OPTIONS pings, not whether the
// gateway registered: the bot's gateway never registers (NOREG), and what
// matters is that the far end answers. It reports ErrUnknownGateway when the
// switch has no gateway by that name — a configuration fault, not an outage.
func (a *Adapter) GatewayUp(name string) (bool, error) {
	out, err := a.api("sofia status gateway %s", name)
	if err != nil {
		return false, err
	}
	if strings.HasPrefix(strings.TrimSpace(out), "Invalid Gateway") {
		return false, fmt.Errorf("%s: %w", name, ErrUnknownGateway)
	}
	return statusField(out, "Status") == "UP", nil
}

// parseProfiles reads the profile rows of the table `sofia status` prints —
// the same table parseTrunks reads for its gateway rows. A running profile's
// state is "RUNNING (n)", n being its call count.
func parseProfiles(out string) []SwitchProfile {
	var profiles []SwitchProfile
	for _, line := range strings.Split(out, "\n") {
		cols := strings.Split(line, "\t")
		if len(cols) < 4 || strings.TrimSpace(cols[1]) != "profile" {
			continue
		}
		profiles = append(profiles, SwitchProfile{
			Name:      strings.TrimSpace(cols[0]),
			IsRunning: strings.HasPrefix(strings.TrimSpace(cols[3]), "RUNNING"),
		})
	}
	return profiles
}

// statusField reads one "Key<padding>\tvalue" line from the detail the switch
// prints for a single profile or gateway. The key is matched whole, so
// "RTP-IP" does not find "Ext-RTP-IP".
func statusField(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "\t")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
