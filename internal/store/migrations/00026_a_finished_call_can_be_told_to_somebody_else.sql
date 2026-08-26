-- SPDX-License-Identifier: Apache-2.0
-- Webhook subscriptions and the outbox that serves them (design 09).
--
-- A customer's system can already place a call and end one. What it could not
-- do was learn what happened without polling. A subscription names an endpoint
-- and a filter; every finished call whose CDR matches is delivered there.
--
-- Two tables rather than one, because they have different lifetimes: the
-- subscription is configuration an operator keeps, the deliveries are a work
-- queue that is swept (FAILED after 30 days, DELIVERED after 7). Merging them
-- would make retention of the second a problem for the first.
--
-- The unique key on deliveries is (subscription_id, call_id, revision), and
-- each of the three earns its place. subscription_id, because one call fans
-- out to every subscription whose filter matches and those rows are
-- independent — one endpoint being down must not hold up another. call_id,
-- because that is what a delivery is about. revision, because InsertCDR
-- replaces a row that saw less of the call: a restart can kill the bot leg,
-- write the call off short, and the fuller row arrives later. When that
-- happens after the short one was already delivered, the correction is sent as
-- revision 2 rather than silently swallowed, and the receiver's rule is
-- last-revision-wins per call.
--
-- call_id is deliberately not a foreign key to cdrs. The delivery records what
-- was sent, and it has to survive the CDR being swept out from under it.

-- +goose Up
CREATE TABLE webhook_subscriptions (
    subscription_id uuid PRIMARY KEY,
    name            text NOT NULL,
    url             text NOT NULL,
    filter          jsonb NOT NULL DEFAULT '{}',
    -- The customer's own credential, presented as a bearer token on every
    -- delivery. Stored recoverably, unlike a password, because it must be sent
    -- verbatim rather than compared against. It points outward: this is never
    -- AICC_API_KEY, which points in.
    auth_token      text NOT NULL DEFAULT '',
    is_enabled      boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE webhook_deliveries (
    delivery_id      uuid PRIMARY KEY,
    subscription_id  uuid NOT NULL REFERENCES webhook_subscriptions (subscription_id) ON DELETE CASCADE,
    call_id          uuid NOT NULL,
    revision         int NOT NULL DEFAULT 1,
    payload          jsonb NOT NULL,
    status           text NOT NULL DEFAULT 'PENDING'
        CONSTRAINT webhook_deliveries_status_check
        CHECK (status IN ('PENDING', 'DELIVERED', 'FAILED')),
    attempt_count    int NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    last_status_code int,
    last_error       text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    delivered_at     timestamptz,
    UNIQUE (subscription_id, call_id, revision)
);

-- The worker's claim: the oldest thing due.
CREATE INDEX idx_webhook_deliveries_due
    ON webhook_deliveries (next_attempt_at)
    WHERE status = 'PENDING';

-- The enqueue's own lookup, and an operator asking what a subscriber was told
-- about one call.
CREATE INDEX idx_webhook_deliveries_call
    ON webhook_deliveries (subscription_id, call_id);

-- Newest first, per subscription, for the read-only screen.
CREATE INDEX idx_webhook_deliveries_recent
    ON webhook_deliveries (subscription_id, created_at DESC);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_subscriptions;
