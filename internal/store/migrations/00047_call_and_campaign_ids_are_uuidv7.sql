-- SPDX-License-Identifier: Apache-2.0
-- Every call id and campaign id the server writes is a UUIDv7 (#400).
--
-- campaigns.id has been a v7 since the table was created, so a plain CHECK
-- holds for every row there. It is NOT VALID only to skip the scan; no
-- existing row can violate it.
--
-- cdrs.call_id and campaign_attempts.call_id are different. Rows written by
-- earlier versions carry v4 ids, and customers reference those ids through
-- webhooks, so they cannot be rewritten. A CHECK, even NOT VALID, is still
-- evaluated on every later INSERT and every UPDATE of the row, and these
-- tables update legacy rows (the CDR upsert ON CONFLICT (call_id) DO UPDATE,
-- cdrs.has_recording, the attempt job/lead/finish updates). So the rule is a
-- trigger that looks only at an INSERT and at an UPDATE that changes call_id;
-- a legacy v4 row stays writable.
--
-- A BEFORE INSERT row trigger also fires for INSERT ... ON CONFLICT DO UPDATE,
-- before the conflict is known. An INSERT whose call_id already exists in the
-- table is therefore let through: it is about to become the update of a
-- legacy row (a plain duplicate insert fails on the key anyway).
--
-- This is a deliberate exception to the rule that a migration narrowing a
-- constraint rewrites existing rows first: historical v4 call ids cannot be
-- rewritten.
--
-- uuid_extract_version() (PostgreSQL 18) returns NULL for an id that is not
-- of the RFC 9562 variant, which includes the nil UUID; those are rejected.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION enforce_call_id_is_uuidv7() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    already_there boolean;
BEGIN
    IF coalesce(uuid_extract_version(NEW.call_id), 0) = 7 THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE call_id = $1)',
                       TG_TABLE_SCHEMA, TG_TABLE_NAME)
            INTO already_there USING NEW.call_id;
        IF already_there THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION '% call_id % is not a UUIDv7', TG_TABLE_NAME, NEW.call_id
        USING ERRCODE = 'check_violation',
              CONSTRAINT = 'ck_' || TG_TABLE_NAME || '_call_id_uuidv7',
              TABLE = TG_TABLE_NAME;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_cdrs_call_id_uuidv7_insert
    BEFORE INSERT ON cdrs
    FOR EACH ROW EXECUTE FUNCTION enforce_call_id_is_uuidv7();
CREATE TRIGGER trg_cdrs_call_id_uuidv7_update
    BEFORE UPDATE OF call_id ON cdrs
    FOR EACH ROW WHEN (NEW.call_id IS DISTINCT FROM OLD.call_id)
    EXECUTE FUNCTION enforce_call_id_is_uuidv7();

CREATE TRIGGER trg_campaign_attempts_call_id_uuidv7_insert
    BEFORE INSERT ON campaign_attempts
    FOR EACH ROW EXECUTE FUNCTION enforce_call_id_is_uuidv7();
CREATE TRIGGER trg_campaign_attempts_call_id_uuidv7_update
    BEFORE UPDATE OF call_id ON campaign_attempts
    FOR EACH ROW WHEN (NEW.call_id IS DISTINCT FROM OLD.call_id)
    EXECUTE FUNCTION enforce_call_id_is_uuidv7();

ALTER TABLE campaigns
    ADD CONSTRAINT ck_campaigns_id_uuidv7 CHECK (coalesce(uuid_extract_version(id), 0) = 7) NOT VALID;

-- +goose Down
ALTER TABLE campaigns DROP CONSTRAINT ck_campaigns_id_uuidv7;
DROP TRIGGER trg_campaign_attempts_call_id_uuidv7_update ON campaign_attempts;
DROP TRIGGER trg_campaign_attempts_call_id_uuidv7_insert ON campaign_attempts;
DROP TRIGGER trg_cdrs_call_id_uuidv7_update ON cdrs;
DROP TRIGGER trg_cdrs_call_id_uuidv7_insert ON cdrs;
DROP FUNCTION enforce_call_id_is_uuidv7();
