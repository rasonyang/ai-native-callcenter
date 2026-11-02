-- SPDX-License-Identifier: Apache-2.0

-- Subscriptions are configuration; deliveries are a work queue. The queries
-- keep them apart for the same reason the tables do.

-- name: InsertWebhookSubscription :one
INSERT INTO webhook_subscriptions (subscription_id, name, url, filter, auth_token, is_enabled)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- UpdateWebhookSubscription leaves the token alone when none is supplied.
-- sqlc.narg gives a nullable parameter: null means "do not touch", an empty
-- string means "clear it". A single non-null parameter could not tell those
-- apart, and the difference matters — omitting the field in a PUT must not
-- silently disarm a subscription's credential.
-- name: UpdateWebhookSubscription :one
UPDATE webhook_subscriptions
SET name = $2, url = $3, filter = $4, is_enabled = $5,
    auth_token = coalesce(sqlc.narg(auth_token), auth_token),
    updated_at = now()
WHERE subscription_id = $1
RETURNING *;

-- name: DeleteWebhookSubscription :execrows
DELETE FROM webhook_subscriptions WHERE subscription_id = $1;

-- name: GetWebhookSubscription :one
SELECT * FROM webhook_subscriptions WHERE subscription_id = $1;

-- name: ListWebhookSubscriptions :many
SELECT * FROM webhook_subscriptions ORDER BY created_at;

-- EnabledWebhookSubscriptions is what the enqueue consults on every finished
-- call, so it asks for exactly what matching needs and nothing else.
-- name: EnabledWebhookSubscriptions :many
SELECT subscription_id, filter FROM webhook_subscriptions WHERE is_enabled;

-- EnqueueWebhookDelivery writes one delivery. The revision is computed from
-- what is already there rather than passed in: the caller knows a CDR was
-- replaced, not how many times it has been.
--
-- ON CONFLICT DO NOTHING guards the one race that matters — two CDR writes
-- landing at once would otherwise both compute revision 2.
-- name: EnqueueWebhookDelivery :one
INSERT INTO webhook_deliveries (delivery_id, subscription_id, call_id, revision, payload)
VALUES ($1, $2, $3,
        coalesce((SELECT max(revision) + 1 FROM webhook_deliveries
                  WHERE subscription_id = $2 AND call_id = $3), 1),
        $4)
ON CONFLICT (subscription_id, call_id, revision) DO NOTHING
RETURNING *;

-- ReplacePendingWebhookDelivery is the ordinary case for a corrected CDR: the
-- delivery has not gone out yet, so the customer never needs to hear about the
-- shorter story at all. Returns nothing when there is no such row, which is how
-- the caller learns it must enqueue the next revision instead.
--
-- Only a delivery that has never been attempted qualifies (attempt_count = 0).
-- A claimed row stays PENDING while it is in flight and while it waits to
-- retry, and the two cannot be told apart by their columns; either may already
-- have reached the receiver, and the 2xx for the old payload would settle the
-- row and lose the correction (#445). The claim raises attempt_count in the
-- same UPDATE that hands the row out, so under READ COMMITTED the row lock
-- orders the two: a row claimed first no longer matches here, and a row
-- replaced first is skipped by the claim's FOR UPDATE SKIP LOCKED until this
-- transaction commits. An unattempted row already has attempt_count 0, no
-- error and no status code, so only the payload and the due time change.
-- name: ReplacePendingWebhookDelivery :one
UPDATE webhook_deliveries
SET payload = $3, next_attempt_at = now()
WHERE subscription_id = $1 AND call_id = $2 AND status = 'PENDING' AND attempt_count = 0
RETURNING *;

-- ClaimDueWebhookDeliveries takes the oldest work that is due. FOR UPDATE SKIP
-- LOCKED so more than one worker — or one worker with more than one goroutine
-- — never hands the same delivery to a customer twice.
-- name: ClaimDueWebhookDeliveries :many
UPDATE webhook_deliveries
SET attempt_count = attempt_count + 1,
    next_attempt_at = now() + interval '1 hour'
WHERE delivery_id IN (
    SELECT delivery_id FROM webhook_deliveries
    WHERE status = 'PENDING' AND next_attempt_at <= now()
    ORDER BY next_attempt_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- The three settle queries are conditional on the attempt they settle ($2 is
-- the attempt_count the worker claimed). A row whose attempt_count has moved on
-- or whose status is no longer PENDING was settled by something else, and the
-- caller gets zero rows back instead of a silent overwrite.
-- name: MarkWebhookDelivered :execrows
UPDATE webhook_deliveries
SET status = 'DELIVERED', delivered_at = now(), last_status_code = $3, last_error = ''
WHERE delivery_id = $1 AND attempt_count = $2 AND status = 'PENDING';

-- name: RescheduleWebhookDelivery :execrows
UPDATE webhook_deliveries
SET next_attempt_at = $3, last_status_code = $4, last_error = $5
WHERE delivery_id = $1 AND attempt_count = $2 AND status = 'PENDING';

-- name: MarkWebhookFailed :execrows
UPDATE webhook_deliveries
SET status = 'FAILED', last_status_code = $3, last_error = $4
WHERE delivery_id = $1 AND attempt_count = $2 AND status = 'PENDING';

-- name: ListWebhookDeliveries :many
SELECT * FROM webhook_deliveries
WHERE subscription_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- SweepWebhookDeliveries removes settled rows past their own window. PENDING
-- is never swept: a delivery still owed is work, not history, and the retry
-- schedule is what ends it.
-- name: SweepWebhookDeliveries :execrows
DELETE FROM webhook_deliveries
WHERE (status = 'DELIVERED' AND created_at < $1)
   OR (status = 'FAILED'    AND created_at < $2);

-- name: WebhookSubscriptionURLAndToken :one
SELECT url, auth_token FROM webhook_subscriptions WHERE subscription_id = $1;
