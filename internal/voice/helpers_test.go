// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

func unmarshalForTest(data []byte) (*rtp.Packet, error) {
	var packet rtp.Packet
	if err := packet.Unmarshal(data); err != nil {
		return nil, err
	}
	return &packet, nil
}

func dtmfPacket(t *testing.T, timestamp uint32, event []byte) []byte {
	t.Helper()
	packet := packRTPHeader(make([]byte, 0, 64), 1, timestamp, 0x1234, 101)
	return append(packet, event...)
}

func drainDigits(session *RTPSession) []string {
	var digits []string
	for {
		select {
		case d := <-session.DTMF():
			digits = append(digits, d)
		case <-time.After(10 * time.Millisecond):
			return digits
		}
	}
}
