-- SPDX-License-Identifier: Apache-2.0
-- An API key's name holds the 128 characters the contract allows (#370).
--
-- APIKeyWrite.name and APIKeyUpdate.name have said maxLength 128 since keys
-- existed, while 00029 made the column varchar(120). A name of 121 to 128
-- characters was valid by the contract and failed at the insert, so the
-- column is widened to what the contract promises rather than the contract
-- narrowed under its clients. varchar(n) counts characters, as maxLength
-- does, so 128 Chinese characters fit too.
--
-- Widening a varchar is a catalogue change in PostgreSQL: no row is rewritten
-- and nothing existing can violate it.

-- +goose Up
ALTER TABLE api_keys ALTER COLUMN name TYPE varchar(128);

-- +goose Down
-- Narrowing: a name longer than 120 characters is cut to its first 120 before
-- the old width returns, or the ALTER is refused on any database that issued
-- one. Names are not unique, so two keys cut to the same text stay two keys;
-- authentication goes by key_hash and never reads the name.
UPDATE api_keys SET name = left(name, 120) WHERE char_length(name) > 120;
ALTER TABLE api_keys ALTER COLUMN name TYPE varchar(120);
