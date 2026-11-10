// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// spoolProber is what the spool probe asks the switch: telephony.Adapter.
type spoolProber interface {
	SpoolProbeWrite(token string) error
	SpoolProbeRemove(token string) error
}

// spoolFault says which half of the spool arrangement is broken.
type spoolFault int

const (
	// spoolNotClearable: the switch cannot write or remove in the directory
	// it records into, or does not have the script that does.
	spoolNotClearable spoolFault = iota + 1
	// spoolNotShared: the switch wrote its probe and the application does not
	// see it under its own recordings directory.
	spoolNotShared
)

// spoolProbeError is a failed spool probe.
type spoolProbeError struct {
	Fault spoolFault
	Err   error
}

func (e *spoolProbeError) Error() string { return e.Err.Error() }
func (e *spoolProbeError) Unwrap() error { return e.Err }

// probeSpool proves the arrangement S3 with a spool relies on: the switch
// writes a probe file where it records, the application sees it under its own
// dir (so the two share the volume), the switch removes it, and it is gone.
// It leaves nothing behind on any path it can control. Shared by the connect
// check and `aicc doctor`.
func probeSpool(sw spoolProber, dir string, stat func(string) (os.FileInfo, error)) error {
	token, err := telephony.NewSpoolProbeToken()
	if err != nil {
		return err
	}
	seen := filepath.Join(dir, telephony.SpoolProbeFilePrefix+token)
	if err := sw.SpoolProbeWrite(token); err != nil {
		return &spoolProbeError{spoolNotClearable, switchSpoolError("the switch cannot write in its recordings directory", err)}
	}
	if _, err := stat(seen); err != nil {
		// Do not leave the switch's file behind: it is the part of the
		// arrangement that does work.
		_ = sw.SpoolProbeRemove(token)
		return &spoolProbeError{spoolNotShared, fmt.Errorf(
			"the switch wrote a probe file that the application does not see at %s: %w", seen, err)}
	}
	if err := sw.SpoolProbeRemove(token); err != nil {
		return &spoolProbeError{spoolNotClearable, switchSpoolError("the switch cannot remove files from its recordings directory", err)}
	}
	if _, err := stat(seen); err == nil {
		return &spoolProbeError{spoolNotClearable, errors.New("the switch reported the probe file removed but the application still sees it")}
	}
	return nil
}

// switchSpoolError names what failed: a switch without the script is not a
// switch that cannot write, and saying so would send an operator to the
// directory's permissions instead of the image.
func switchSpoolError(what string, err error) error {
	if errors.Is(err, telephony.ErrSpoolScriptMissing) {
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// spoolCheck probes the spool at every connect when the recording backend is
// S3 with a spool. A failure is one loud ERROR and nothing more: calls work
// and recordings are stored; only the spool copy stays.
type spoolCheck struct {
	probe func() error
	dir   string
	log   *slog.Logger
}

func newSpoolCheck(sw spoolProber, dir string, log *slog.Logger) *spoolCheck {
	return &spoolCheck{
		probe: func() error { return probeSpool(sw, dir, os.Stat) },
		dir:   dir,
		log:   log,
	}
}

// onConnect is the link's connect hook.
func (c *spoolCheck) onConnect(ctx context.Context) {
	err := c.probe()
	if err == nil {
		return
	}
	cause := "NOT_CLEARABLE"
	fix := "update the switch image (it needs aicc_spool.lua) and check that the switch can write in its recordings directory"
	var pe *spoolProbeError
	if errors.As(err, &pe) && pe.Fault == spoolNotShared {
		cause = "NOT_SHARED"
		fix = "mount the switch's recordings directory at AICC_RECORDING_DIR in the application"
	}
	c.log.ErrorContext(ctx, "recording spool check failed: spool copies of booked recordings will stay on disk",
		"cause", cause, "dir", c.dir, "error", err, "fix", fix)
}
