-- SPDX-License-Identifier: Apache-2.0
-- A campaign can be paused because the deployment has no outbound endpoint.
--
-- The dialer pauses a RUNNING campaign with OUTBOUND_NOT_CONFIGURED when it
-- finds AICC_OUTBOUND_ENDPOINT empty, a cause an operator fixes in the
-- deployment, not at the carrier (TRUNK_FAILURE). The CHECK installed by 00037
-- only widens, so no existing row needs rewriting going up.

-- +goose Up
ALTER TABLE campaigns DROP CONSTRAINT ck_campaigns_pause_reason;
ALTER TABLE campaigns ADD CONSTRAINT ck_campaigns_pause_reason CHECK (
    pause_reason IS NULL
    OR (status = 'PAUSED' AND pause_reason IN
        ('MANUAL', 'TRUNK_FAILURE', 'PROVIDER_FAILURE', 'OUTBOUND_NOT_CONFIGURED')));

-- +goose Down
-- Narrowing: a row holding the new reason is rewritten to the nearest old one
-- (the deployment's way out is not working) before the constraint returns.
ALTER TABLE campaigns DROP CONSTRAINT ck_campaigns_pause_reason;
UPDATE campaigns SET pause_reason = 'TRUNK_FAILURE' WHERE pause_reason = 'OUTBOUND_NOT_CONFIGURED';
ALTER TABLE campaigns ADD CONSTRAINT ck_campaigns_pause_reason CHECK (
    pause_reason IS NULL
    OR (status = 'PAUSED' AND pause_reason IN ('MANUAL', 'TRUNK_FAILURE', 'PROVIDER_FAILURE')));
