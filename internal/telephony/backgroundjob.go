// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// normalizeBackgroundJob reads the result of a bgapi command. It carries no
// channel, so ChannelID is cleared: the Unique-ID header of this event, where
// present, is not a channel anyone should look up.
func normalizeBackgroundJob(ev *esl.Event, out SwitchEvent) (SwitchEvent, bool) {
	id := strings.TrimSpace(ev.Get("Job-UUID"))
	if id == "" {
		return SwitchEvent{}, false
	}
	out.Kind = KindBackgroundJob
	out.ChannelID = ""
	out.ChannelName = ""
	out.JobID = id
	out.IsJobOK, out.JobCause = ParseJobResult(ev.Body)
	return out, true
}

// maxJobCauseLen bounds a cause that is not a bare name, so a verbose error
// string cannot become a ledger value of any length.
const maxJobCauseLen = 64

// UnspecifiedJobCause stands for a failure whose body names no cause.
const UnspecifiedJobCause = "ORIGINATE_FAILED"

// ParseJobResult reads a background job's body: "+OK <uuid>" succeeded,
// "-ERR <CAUSE>" failed. The cause is the bare name whatever surrounds it:
// leading or trailing space, a trailing newline, text after the name. A
// failure that names no cause is UnspecifiedJobCause. Any other body,
// including an empty one, is neither a success nor a failure.
func ParseJobResult(body string) (isOK bool, cause string) {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	line = strings.TrimSpace(line)
	switch {
	case line == "+OK" || strings.HasPrefix(line, "+OK "):
		return true, ""
	case line == "-ERR" || strings.HasPrefix(line, "-ERR ") || strings.HasPrefix(line, "-ERR\t"):
		fields := strings.Fields(strings.TrimPrefix(line, "-ERR"))
		if len(fields) == 0 {
			return false, UnspecifiedJobCause
		}
		if isCauseName(fields[0]) {
			return false, fields[0]
		}
		joined := strings.Join(fields, " ")
		if len(joined) > maxJobCauseLen {
			joined = joined[:maxJobCauseLen]
		}
		return false, joined
	}
	return false, ""
}

func isCauseName(s string) bool {
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return s != ""
}
