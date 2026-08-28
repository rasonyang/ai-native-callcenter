-- SPDX-License-Identifier: Apache-2.0
-- Contact phone numbers are stored in the form everything compares them in.
--
-- The application now strips separators and requires +?digits before storing
-- a number (uniqueness, the caller-card ANI lookup and click-to-dial all
-- compare the column literally). Rows written before that rule keep whatever
-- was typed, so "186-8888 6666" and 18688886666 could sit as two contacts and
-- the lookup between them was a coin toss.
--
-- Existing rows are normalized where that does not collide with a row already
-- holding the normalized form; a collision is left as it was, because merging
-- two contacts is a judgement about people, not a schema's to make. Rows that
-- are not +?digits even after stripping (letters, empty) are also left: the
-- application refuses new ones, and deleting history is not this migration's
-- call either.

-- +goose Up
UPDATE contacts c
SET phone_number = regexp_replace(c.phone_number, '[ ().-]', '', 'g')
WHERE c.phone_number ~ '[ ().-]'
  AND regexp_replace(c.phone_number, '[ ().-]', '', 'g') ~ '^\+?[0-9]{2,32}$'
  AND NOT EXISTS (
    SELECT 1 FROM contacts other
    WHERE other.id <> c.id
      AND other.phone_number = regexp_replace(c.phone_number, '[ ().-]', '', 'g'));

-- +goose Down
-- The stripped formatting is not recoverable, and does not need to be: the
-- normalized value is a valid spelling of the same number under the old rule.
SELECT 1;
