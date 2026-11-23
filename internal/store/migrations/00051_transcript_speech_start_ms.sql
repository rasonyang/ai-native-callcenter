-- SPDX-License-Identifier: Apache-2.0
-- +goose Up

-- A caller line's occurredAt is when the line arrived — and the transcription
-- trails the speech (and can trail the bot's reaction to it). speech_start_ms
-- is the caller's speaking time as an offset from answered_at, beside
-- offset_ms, when the provider reported it; NULL means nobody knew (#587).
ALTER TABLE transcripts ADD COLUMN speech_start_ms int;

-- +goose Down

ALTER TABLE transcripts DROP COLUMN speech_start_ms;
