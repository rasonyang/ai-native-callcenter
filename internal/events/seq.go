// SPDX-License-Identifier: Apache-2.0

package events

import (
	"context"
	"fmt"
	"sync"
)

// blockSize is how many sequence numbers one database round trip reserves.
// PostgreSQL therefore never sits on the per-event path; a crash burns the
// remainder of the block, which is why gaps in seq are meaningless.
const blockSize = 100_000

// SeqReserver reserves sequence blocks durably.
type SeqReserver interface {
	// ReserveSeqBlock advances the named counter by size and returns the new
	// value, i.e. the exclusive upper bound of the reserved block.
	ReserveSeqBlock(ctx context.Context, name string, size int64) (int64, error)
}

// Sequence hands out globally increasing event ids.
//
// Allocation never fails at the call site: if the database is unreachable the
// sequence keeps counting in memory, because losing the event stream is worse
// than losing gap-free numbering (which is not promised anyway).
type Sequence struct {
	reserver SeqReserver
	name     string

	mu   sync.Mutex
	next int64 // next id to hand out
	end  int64 // exclusive upper bound of the current block
}

// NewSequence builds a Sequence backed by reserver.
func NewSequence(reserver SeqReserver, name string) *Sequence {
	return &Sequence{reserver: reserver, name: name}
}

// Next returns the next sequence number, reserving a new block when needed.
// It reports whether the value is durable; an in-memory fallback returns false.
func (s *Sequence) Next(ctx context.Context) (id int64, durable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.next < s.end {
		s.next++
		return s.next, true
	}

	upper, err := s.reserver.ReserveSeqBlock(ctx, s.name, blockSize)
	if err != nil {
		// Degrade rather than stall the publisher.
		s.next++
		if s.next > s.end {
			s.end = s.next
		}
		return s.next, false
	}

	s.next = upper - blockSize + 1
	s.end = upper
	return s.next, true
}

// Current reports the last handed-out id, for diagnostics.
func (s *Sequence) Current() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

func (s *Sequence) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("seq(%s next=%d end=%d)", s.name, s.next, s.end)
}
