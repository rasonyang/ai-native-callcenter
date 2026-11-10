// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// diskProber plays the switch against a real directory.
type diskProber struct {
	dir      string
	writeErr error
	hide     bool
	keep     bool
	removed  int
}

func (p *diskProber) file(token string) string {
	return filepath.Join(p.dir, telephony.SpoolProbeFilePrefix+token)
}

func (p *diskProber) SpoolProbeWrite(token string) error {
	if p.writeErr != nil {
		return p.writeErr
	}
	if p.hide {
		return nil
	}
	return os.WriteFile(p.file(token), []byte("x"), 0o600)
}

func (p *diskProber) SpoolProbeRemove(token string) error {
	p.removed++
	if p.keep {
		return nil
	}
	_ = os.Remove(p.file(token))
	return nil
}

func TestProbeSpoolFaults(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		p     func(dir string) *diskProber
		fault spoolFault
	}{
		"healthy":         {func(d string) *diskProber { return &diskProber{dir: d} }, 0},
		"cannot write":    {func(d string) *diskProber { return &diskProber{dir: d, writeErr: errors.New("EACCES")} }, spoolNotClearable},
		"not shared":      {func(d string) *diskProber { return &diskProber{dir: d, hide: true} }, spoolNotShared},
		"remove is a lie": {func(d string) *diskProber { return &diskProber{dir: d, keep: true} }, spoolNotClearable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := tc.p(dir)
			err := probeSpool(p, dir, os.Stat)
			var pe *spoolProbeError
			switch {
			case tc.fault == 0 && err != nil:
				t.Fatal(err)
			case tc.fault != 0 && (!errors.As(err, &pe) || pe.Fault != tc.fault):
				t.Fatalf("err = %v, want fault %d", err, tc.fault)
			}
			if tc.fault == spoolNotShared && p.removed == 0 {
				t.Error("the switch's unseen probe file was not cleaned up")
			}
		})
	}
}

// A switch without the script is reported as that, not as a switch that
// cannot write: the operator's fix is the image, not the directory.
func TestProbeSpoolNamesAMissingScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := fmt.Errorf("aicc_spool.lua probe-write: reply %q: %w", "-ERR Cannot execute script", telephony.ErrSpoolScriptMissing)
	err := probeSpool(&diskProber{dir: dir, writeErr: missing}, dir, os.Stat)
	var pe *spoolProbeError
	if !errors.As(err, &pe) || pe.Fault != spoolNotClearable {
		t.Fatalf("err = %v, want fault NOT_CLEARABLE", err)
	}
	if !errors.Is(err, telephony.ErrSpoolScriptMissing) || strings.Contains(err.Error(), "cannot write") {
		t.Errorf("err = %q, want the missing script named and no claim about writing", err)
	}
}

func TestSpoolCheckLogsOneGreppableErrorAndStaysQuietWhenHealthy(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dir := t.TempDir()

	newSpoolCheck(&diskProber{dir: dir}, dir, log).onConnect(context.Background())
	if logs.Len() != 0 {
		t.Errorf("a healthy spool logged %q", logs.String())
	}

	newSpoolCheck(&diskProber{dir: dir, writeErr: errors.New("-ERR WRITE_FAILED")}, dir, log).onConnect(context.Background())
	if got := logs.String(); !strings.Contains(got, "level=ERROR") ||
		!strings.Contains(got, "recording spool check failed") || !strings.Contains(got, "cause=NOT_CLEARABLE") {
		t.Errorf("log = %q", got)
	}

	logs.Reset()
	newSpoolCheck(&diskProber{dir: dir, hide: true}, dir, log).onConnect(context.Background())
	if !strings.Contains(logs.String(), "cause=NOT_SHARED") {
		t.Errorf("log = %q", logs.String())
	}
}
