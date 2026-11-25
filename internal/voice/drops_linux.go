// SPDX-License-Identifier: Apache-2.0

//go:build linux

package voice

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// socketDrops reads the drops column of the /proc/net/udp row for a socket
// bound to localPort: datagrams the kernel discarded because the receive
// buffer was full (#597). It is cumulative since the socket was opened.
// -1 when the port has no row.
func socketDrops(localPort int) int64 {
	data, err := os.ReadFile("/proc/net/udp")
	if err != nil {
		return -1
	}
	want := fmt.Sprintf(":%04X", localPort)
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasSuffix(fields[1], want) {
			continue
		}
		if drops, err := strconv.ParseInt(fields[len(fields)-1], 10, 64); err == nil {
			return drops
		}
		return -1
	}
	return -1
}
