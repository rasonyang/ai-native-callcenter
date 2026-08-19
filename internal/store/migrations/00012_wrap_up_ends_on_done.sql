-- SPDX-License-Identifier: Apache-2.0
-- After-call work ends when the agent files it, not when a clock says so
-- (owner directive 2026-08-19).
--
-- 00010 gave after-call work a countdown per agent and a two-level vocabulary,
-- both taken from the reference mock. The directive is narrower and better:
-- one required disposition, an optional note, a Done button, and the agent
-- stays out of routing until they press it. A deadline contradicts a required
-- disposition — an agent who waits it out files nothing — so the deadline goes,
-- and with it every column that only existed to serve one:
--
--   agents.wrap_up_time_sec     how long the countdown ran
--   agent_states.wrap_up_ends_at when it would have expired
--
-- The vocabulary flattens to the four words the directive names. Categories
-- were a grouping the agent had to navigate before reaching the word they
-- wanted; with four words there is nothing to group.
--
-- Wrap-ups already filed keep their `disposition_code` and the label captured
-- with them, even where that code has just left the vocabulary. That is the
-- point of capturing a label: history says what was filed, not what the list
-- says today. Only the category columns go, because a flat vocabulary has no
-- category to name.

-- +goose Up

ALTER TABLE agents DROP COLUMN wrap_up_time_sec;
ALTER TABLE agent_states DROP COLUMN wrap_up_ends_at;

ALTER TABLE wrap_ups DROP COLUMN category_code;
ALTER TABLE wrap_ups DROP COLUMN category_label;

-- Dropping the column takes its foreign key and its index with it; naming
-- either one here fails on the second statement, which is the kind of thing
-- only a server ever tells you.
ALTER TABLE dispositions DROP COLUMN category_code;
DROP TABLE disposition_categories;

DELETE FROM dispositions;
INSERT INTO dispositions (code, label, position) VALUES
    ('RESOLVED',           'Resolved',           1),
    ('FOLLOW_UP_REQUIRED', 'Follow-up Required', 2),
    ('NO_ANSWER',          'No Answer',          3),
    ('OTHER',              'Other',              4);

CREATE INDEX idx_dispositions_position ON dispositions (position);

-- +goose Down
DROP INDEX idx_dispositions_position;

CREATE TABLE disposition_categories (
    code     varchar(64) PRIMARY KEY,
    label    text NOT NULL,
    position int NOT NULL DEFAULT 0
);

INSERT INTO disposition_categories (code, label, position) VALUES
    ('RESOLVED',  'Resolved',  1),
    ('FOLLOW_UP', 'Follow-up', 2),
    ('OTHER',     'Other',     3);

DELETE FROM dispositions;
ALTER TABLE dispositions ADD COLUMN category_code varchar(64) NOT NULL DEFAULT 'OTHER';
ALTER TABLE dispositions ALTER COLUMN category_code DROP DEFAULT;
ALTER TABLE dispositions ADD CONSTRAINT fk_dispositions_disposition_categories
    FOREIGN KEY (category_code) REFERENCES disposition_categories (code) ON DELETE CASCADE;
CREATE INDEX idx_dispositions_category_code_position ON dispositions (category_code, position);

INSERT INTO dispositions (code, category_code, label, position) VALUES
    ('ANSWERED_QUESTION',  'RESOLVED',  'Answered question',  1),
    ('ISSUE_FIXED',        'RESOLVED',  'Issue fixed',        2),
    ('REFUND_PROCESSED',   'RESOLVED',  'Refund processed',   3),
    ('CALLBACK_SCHEDULED', 'FOLLOW_UP', 'Callback scheduled', 1),
    ('ESCALATED',          'FOLLOW_UP', 'Escalated',          2),
    ('PENDING_CUSTOMER',   'FOLLOW_UP', 'Pending customer',   3),
    ('WRONG_NUMBER',       'OTHER',     'Wrong number',       1),
    ('NO_AUDIO',           'OTHER',     'No audio',           2),
    ('SPAM_CALL',          'OTHER',     'Spam call',          3);

ALTER TABLE wrap_ups ADD COLUMN category_label text NOT NULL DEFAULT '';
ALTER TABLE wrap_ups ADD COLUMN category_code text NOT NULL DEFAULT '';

ALTER TABLE agent_states ADD COLUMN wrap_up_ends_at timestamptz;
ALTER TABLE agents ADD COLUMN wrap_up_time_sec int NOT NULL DEFAULT 30
    CHECK (wrap_up_time_sec >= 0);
