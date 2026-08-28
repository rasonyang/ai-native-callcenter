// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/pressly/goose/v3"
)

// A contact's number is stored in the form everything compares it in: what
// the trunk presents as ANI and what click-to-dial sends. Formatting people
// paste is stripped; what remains must be +?digits, or the caller card could
// never find the contact and the dial button could never ring them. Needs a
// real server (see migrate_test.go); without AICC_TEST_DATABASE_URL it skips.
func TestAContactsNumberIsStoredDialable(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	book := st.Contacts()

	created, err := book.Create(ctx, ContactWrite{PhoneNumber: " 186-8888 (6666) "}, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.PhoneNumber != "18688886666" {
		t.Errorf("stored %q, want 18688886666 — separators are formatting, not identity", created.PhoneNumber)
	}

	// The same number in different clothes is the same contact.
	if _, err := book.Create(ctx, ContactWrite{PhoneNumber: "186 8888-6666"}, nil); !errors.Is(err, ErrContactExists) {
		t.Errorf("differently formatted duplicate: got %v, want ErrContactExists", err)
	}

	for _, bad := range []string{"abc", "186#8888", "1", "+"} {
		if _, err := book.Create(ctx, ContactWrite{PhoneNumber: bad}, nil); !errors.Is(err, ErrContactInvalid) {
			t.Errorf("create(%q): got %v, want ErrContactInvalid", bad, err)
		}
	}

	// An internal extension is a legitimate contact.
	if _, err := book.Create(ctx, ContactWrite{PhoneNumber: "1001"}, nil); err != nil {
		t.Errorf("an extension must be storable: %v", err)
	}
}
