// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// #367: the eight-character floor counts characters (code points), as the API
// contract's minLength does. Counted in bytes, seven Chinese characters
// (21 bytes) passed. Both are refused before storage is touched, which is why
// a Service with no database can answer them.
func TestAPasswordShortInCharactersIsRefusedHoweverManyBytesItIs(t *testing.T) {
	t.Parallel()
	for _, password := range []string{
		strings.Repeat("话", 7), // 21 bytes
		"📞话📞话📞话📞",              // 25 bytes
		strings.Repeat("📞", 3), // 12 bytes
	} {
		t.Run(password, func(t *testing.T) {
			t.Parallel()
			s := &Service{}

			_, err := s.CreateUser(context.Background(), NewUser{
				Username: "novanet.agent", Password: password, Role: RoleAgent,
			})
			if !errors.Is(err, ErrValidation) {
				t.Errorf("CreateUser() error = %v, want ErrValidation", err)
			}
			if err := s.SetPassword(context.Background(), uuid.New(), password); !errors.Is(err, ErrValidation) {
				t.Errorf("SetPassword() error = %v, want ErrValidation", err)
			}
		})
	}
}
