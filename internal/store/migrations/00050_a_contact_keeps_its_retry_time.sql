-- SPDX-License-Identifier: Apache-2.0
-- A contact keeps the time its retry policy gave it apart from the time a
-- calling window moved it to (docs/design/10-campaigns.md sections 3.2 and
-- 8.1, #616).
--
-- campaign_contacts.next_attempt_at is when a contact may be dialled next.
-- After an attempt the finish routine sets it to the call's end plus the
-- outcome's interval, moved to the next opening of the contact's calling
-- window when that falls outside it, and the dialer moves a due contact whose
-- window is closed to the window's next opening. Once moved, the stored time
-- no longer says what the retry policy asked for, so a window widened after it
-- closed could not tell a contact waiting out its retry interval from one
-- waiting only for the old window, and kept every contact with a history at
-- the old window's opening.
--
-- retry_at is the policy's time: written by the finish routine with
-- next_attempt_at, before the window moves it, and left alone by every
-- deferral (a calling window's, the frequency window's, the daily cap's).
-- NULL means the contact has no policy time and is due as soon as a window
-- allows: never tried, voided, or in flight (the claim clears it). The re-arm
-- run when a window changes judges a waiting contact again from the later of
-- now and retry_at, never earlier. Like next_attempt_at it is set only on an
-- ACTIVE contact, so the CHECK.
--
-- Backfill: the policy's time of a contact that was already waiting cannot be
-- recovered exactly. Recomputing it from the latest attempt's finish and
-- outcome would read the retry policy as it is now, and an operator may have
-- changed it since the contact settled. So an ACTIVE contact with a history
-- that is waiting gets its current due time, which is never earlier than its
-- policy's: a window change after the upgrade cannot shorten its interval, and
-- at worst such a contact waits for the time it was already due at. A contact
-- never tried keeps NULL, which is exact.

-- +goose Up
ALTER TABLE campaign_contacts ADD COLUMN retry_at timestamptz;
UPDATE campaign_contacts SET retry_at = next_attempt_at
WHERE status = 'ACTIVE' AND last_outcome IS NOT NULL AND next_attempt_at IS NOT NULL;
ALTER TABLE campaign_contacts ADD CONSTRAINT ck_campaign_contacts_retry_at
    CHECK (retry_at IS NULL OR status = 'ACTIVE');

-- +goose Down
ALTER TABLE campaign_contacts DROP CONSTRAINT ck_campaign_contacts_retry_at;
ALTER TABLE campaign_contacts DROP COLUMN retry_at;
