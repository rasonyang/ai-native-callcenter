-- SPDX-License-Identifier: Apache-2.0
-- After-call work becomes something an agent can actually file.
--
-- It existed as a presence state and nothing else: the FSM could enter
-- AFTER_CALL_WORK, the timer could leave it, and no call, disposition or note
-- was ever recorded. `cdrs.disposition` was a column nothing wrote. This
-- migration gives the state something to say.
--
-- Wrap-ups are their own table rather than columns on `cdrs` because the CDR
-- row is written when the *call* ends, and an agent finishes their part of a
-- call before that — a transfer hands the caller on and the agent wraps up
-- while the conversation continues. An UPDATE against a row that does not
-- exist yet would land nowhere; a row keyed by (call_id, agent_id) lands
-- whichever arrives first, and one call may be wrapped up by every agent who
-- was on it.
--
-- Labels are captured on the wrap-up at filing time. The vocabulary is edited
-- by people; the ledger is history, and history that changes meaning when a
-- category is renamed is not history.

-- +goose Up

ALTER TABLE agent_states ADD COLUMN wrap_up_call_id uuid;

CREATE TABLE disposition_categories (
    code     varchar(64) PRIMARY KEY,
    label    text NOT NULL,
    position int NOT NULL DEFAULT 0
);

CREATE TABLE dispositions (
    code          varchar(64) PRIMARY KEY,
    category_code varchar(64) NOT NULL,
    label         text NOT NULL,
    position      int NOT NULL DEFAULT 0,
    is_enabled    boolean NOT NULL DEFAULT true,
    CONSTRAINT fk_dispositions_disposition_categories FOREIGN KEY (category_code)
        REFERENCES disposition_categories (code) ON DELETE CASCADE
);

CREATE INDEX idx_dispositions_category_code_position ON dispositions (category_code, position);

-- A starting vocabulary, so a fresh deployment can complete after-call work on
-- its first call. An installation with nothing to file under cannot file at
-- all. Codes are stable identifiers; labels are what agents read and are
-- theirs to change.
INSERT INTO disposition_categories (code, label, position) VALUES
    ('RESOLVED',  'Resolved',  1),
    ('FOLLOW_UP', 'Follow-up', 2),
    ('OTHER',     'Other',     3);

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

CREATE TABLE wrap_ups (
    call_id           uuid NOT NULL,
    agent_id          uuid NOT NULL,
    disposition_code  text NOT NULL DEFAULT '',
    disposition_label text NOT NULL DEFAULT '',
    category_code     text NOT NULL DEFAULT '',
    category_label    text NOT NULL DEFAULT '',
    note              text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (call_id, agent_id)
);

CREATE INDEX idx_wrap_ups_agent_id_created_at ON wrap_ups (agent_id, created_at DESC);

-- +goose Down
DROP TABLE wrap_ups;
DROP TABLE dispositions;
DROP TABLE disposition_categories;
ALTER TABLE agent_states DROP COLUMN wrap_up_call_id;
