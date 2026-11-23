// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package voice

// socketDrops has no portable source; other kernels do not publish the
// per-socket overflow count the way Linux's /proc/net/udp does. The watch
// reports -1 and the UAS keeps the metric silent.
func socketDrops(localPort int) int64 {
	return -1
}
