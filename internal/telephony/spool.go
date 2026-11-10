// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/recording"
)

// The recording spool, from the switch's side.
//
// With the S3 recording backend and a spool, the switch creates the dated
// spool directories itself with a fixed mode (0750), so the application can
// read a recording but never unlink it. The switch owns every file and
// directory in the spool, so the application asks it to do the removal:
// freeswitch/scripts/aicc_spool.lua, run through mod_lua's lua API command.
//
// These commands use the blocking api path, not bgapi. The caller needs the
// script's reply inline, the Adapter has no generic wait for a
// BACKGROUND_JOB, and the script makes a few filesystem calls. They join the
// writes that block the link's one connection (#407).

// spoolScript is the script's name; mod_lua resolves it against the switch's
// script directory.
const spoolScript = "aicc_spool.lua"

// SpoolProbeFilePrefix names the probe file at the spool root. It is not a
// recording key, so the spool sweep never lists one.
const SpoolProbeFilePrefix = ".aicc-spool-probe-"

var spoolProbeToken = regexp.MustCompile(`^[0-9a-f]{16}$`)

// NewSpoolProbeToken is a fresh probe token: 16 lowercase hex digits.
func NewSpoolProbeToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ReleaseSpoolFile asks the switch to remove the spool copy of a recording
// and the empty dated directories it leaves. A copy already gone is success.
// The key is checked here as well as in the script: nothing but a recording
// key is ever put on the command line.
func (a *Adapter) ReleaseSpoolFile(key string) error {
	if _, ok := recording.CallIDOf(key); !ok {
		return fmt.Errorf("spool release: %q is not a recording key", key)
	}
	return a.spool("release", key)
}

// SpoolProbeWrite asks the switch to create the probe file for token at the
// spool root, and SpoolProbeRemove to remove it. Together they prove the
// switch can write and remove where it records, and, with a look at the
// application's own mount in between, that both see the same directory.
func (a *Adapter) SpoolProbeWrite(token string) error {
	if !spoolProbeToken.MatchString(token) {
		return fmt.Errorf("spool probe: %q is not a probe token", token)
	}
	return a.spool("probe-write", token)
}

// SpoolProbeRemove removes the probe file; see SpoolProbeWrite.
func (a *Adapter) SpoolProbeRemove(token string) error {
	if !spoolProbeToken.MatchString(token) {
		return fmt.Errorf("spool probe: %q is not a probe token", token)
	}
	return a.spool("probe-remove", token)
}

// ErrSpoolScriptMissing is a reply that did not come from aicc_spool.lua:
// what a switch whose image predates the script answers (mod_lua's "-ERR
// Cannot execute script", or nothing at all).
var ErrSpoolScriptMissing = errors.New("the switch does not run " + spoolScript + " (update the switch image)")

// spoolCauses are the causes aicc_spool.lua refuses with. A -ERR naming any
// other is mod_lua's, not the script's.
var spoolCauses = map[string]bool{
	"NOT_LOCAL_SPOOL": true, "BAD_COMMAND": true, "BAD_KEY": true,
	"BAD_TOKEN": true, "REMOVE_FAILED": true, "WRITE_FAILED": true,
}

// spool runs one aicc_spool.lua command and reads its reply: "+OK", or
// "-ERR <CAUSE> <detail>". Any other reply, an empty one included, is what a
// switch whose image lacks the script answers.
func (a *Adapter) spool(command, arg string) error {
	reply, err := a.api("lua %s %s %s", spoolScript, command, arg)
	if err != nil {
		return fmt.Errorf("%s %s: %w", spoolScript, command, err)
	}
	reply = strings.TrimSpace(reply)
	if reply == "+OK" {
		return nil
	}
	if rest, ok := strings.CutPrefix(reply, "-ERR "); ok {
		if cause, _, _ := strings.Cut(rest, " "); spoolCauses[cause] {
			return fmt.Errorf("%s %s: the switch refused: %s", spoolScript, command, rest)
		}
	}
	return fmt.Errorf("%s %s: reply %q: %w", spoolScript, command, reply, ErrSpoolScriptMissing)
}
