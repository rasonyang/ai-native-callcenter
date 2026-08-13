// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"fmt"
	"strings"
)

// Registration is one endpoint the switch currently knows about.
type Registration struct {
	Extension string
	// IsReachable reports whether the endpoint answers the switch's keepalive.
	// A registration that stops answering is the failure that matters: a
	// crashed browser tab still looks registered.
	IsReachable bool
}

// Registrations asks the switch which endpoints are registered right now.
//
// Live sofia events only tell us about changes, so an application that starts
// after the phones did would believe every agent's phone is missing. This is
// the reconciliation that closes that gap, on startup and on every reconnect.
func (a *Adapter) Registrations(profile string) ([]Registration, error) {
	if profile == "" {
		profile = "internal"
	}
	out, err := a.cmd.API(fmt.Sprintf("sofia status profile %s reg", profile))
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	return parseRegistrations(out), nil
}

// parseRegistrations reads sofia's registration listing. The format is a
// sequence of "Field: value" lines per registration, separated by rules.
func parseRegistrations(out string) []Registration {
	var (
		result  []Registration
		current *Registration
	)

	flush := func() {
		if current != nil && current.Extension != "" {
			result = append(result, *current)
		}
		current = nil
	}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Call-ID:"):
			// Each registration block starts with its call id.
			flush()
			current = &Registration{IsReachable: true}
		case strings.HasPrefix(line, "User:"):
			if current == nil {
				current = &Registration{IsReachable: true}
			}
			user := strings.TrimSpace(strings.TrimPrefix(line, "User:"))
			if at := strings.Index(user, "@"); at > 0 {
				user = user[:at]
			}
			current.Extension = user
		case strings.HasPrefix(line, "Ping-Status:"):
			if current != nil {
				status := strings.TrimSpace(strings.TrimPrefix(line, "Ping-Status:"))
				// Sofia reports Reachable, Unreachable, or nothing useful when
				// pinging is disabled; only an explicit failure counts.
				current.IsReachable = !strings.EqualFold(status, "Unreachable")
			}
		}
	}
	flush()
	return result
}
