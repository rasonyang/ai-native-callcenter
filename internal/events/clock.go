// SPDX-License-Identifier: Apache-2.0

package events

import "time"

// nowFunc is the clock used to stamp envelopes; tests replace it.
var nowFunc = func() time.Time { return time.Now().UTC() }
