// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/rasonyang/ai-native-callcenter/internal/httpapi"
)

// switchUUIDCheck asserts, at every connect, that the switch mints v7 UUIDs.
//
// The dialplan mints the call id of every inbound DID call on the switch, and
// the database refuses a call id that is not v7. FreeSWITCH 1.10 ignores
// uuid-version=7 and mints v4, so the configuration's word is not enough: the
// application asks. The result is reported by /readyz (a violation fails it).
// Anything short of an answer (not asked yet, link lost, command failed) is
// unknown, which never fails readiness.
type switchUUIDCheck struct {
	// version asks the switch to mint a UUID and returns its version.
	version func() (int, error)
	log     *slog.Logger

	mu     sync.Mutex
	status httpapi.SwitchUUIDStatus
}

func newSwitchUUIDCheck(version func() (int, error), log *slog.Logger) *switchUUIDCheck {
	return &switchUUIDCheck{version: version, log: log}
}

// Status is the latest result; safe for concurrent use.
func (c *switchUUIDCheck) Status() httpapi.SwitchUUIDStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *switchUUIDCheck) set(s httpapi.SwitchUUIDStatus) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

// onConnect is the link's connect hook.
func (c *switchUUIDCheck) onConnect(ctx context.Context) {
	v, err := c.version()
	if err != nil {
		c.set(httpapi.SwitchUUIDStatus{})
		c.log.WarnContext(ctx, "cannot check the UUID version the switch mints", "error", err)
		return
	}
	c.set(httpapi.SwitchUUIDStatus{Checked: true, Version: v})
	if v != 7 {
		c.log.ErrorContext(ctx, "the switch mints UUIDs that are not v7: every inbound call id would be refused by the database",
			"version", v,
			"fix", "set uuid-version=7 in switch.conf.xml; it needs FreeSWITCH 1.11 or later (1.10 ignores it)")
	}
}

// onLost is the link's lost hook: with no link there is nothing to vouch for.
func (c *switchUUIDCheck) onLost() { c.set(httpapi.SwitchUUIDStatus{}) }
