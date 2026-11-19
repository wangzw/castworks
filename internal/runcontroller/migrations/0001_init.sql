-- runcontroller 的持久化表结构。列见 docs/milestone/v0.1.0/runcontroller.md
-- “数据”一节；state 与 substate 的合法组合照同文档“状态机”一节。

CREATE TABLE run (
    run_id               text PRIMARY KEY,
    state                text NOT NULL,
    substate             text NOT NULL DEFAULT '',
    actor_name           text NOT NULL,
    next_inbox_seq       bigint NOT NULL,
    delivered_through    bigint NOT NULL DEFAULT 0,
    idle_since           timestamptz,
    provision_returned_at timestamptz,
    failure_reason       text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL,
    ended_at             timestamptz,
    CONSTRAINT run_state_valid CHECK (state IN (
        'provisioning', 'active', 'suspending', 'suspended', 'resuming', 'ended', 'failed'
    )),
    CONSTRAINT run_state_substate_valid CHECK (
        (state = 'active' AND substate IN ('in_turn', 'delivering', 'boundary'))
        OR (state IN ('suspended', 'resuming') AND substate = 'snapshotted')
        OR (state IN ('provisioning', 'suspending', 'ended', 'failed') AND substate = '')
    ),
    CONSTRAINT run_next_inbox_seq_valid CHECK (next_inbox_seq >= 1),
    CONSTRAINT run_delivered_through_valid CHECK (delivered_through >= 0)
);

CREATE TABLE inbox (
    run_id     text NOT NULL REFERENCES run (run_id),
    inbox_seq  bigint NOT NULL,
    text       text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (run_id, inbox_seq),
    CONSTRAINT inbox_seq_valid CHECK (inbox_seq >= 1),
    CONSTRAINT inbox_text_valid CHECK (octet_length(text) BETWEEN 1 AND 65536)
);

CREATE TABLE turn (
    run_id      text NOT NULL REFERENCES run (run_id),
    turn_id     text NOT NULL,
    delivery_id text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL,
    ended_at    timestamptz,
    output      text NOT NULL DEFAULT '',
    is_error    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, turn_id)
);

CREATE TABLE suspension (
    run_id             text NOT NULL REFERENCES run (run_id),
    seq                bigint NOT NULL,
    started_at         timestamptz NOT NULL,
    snapshot_ms        bigint,
    snapshot_bytes     bigint,
    resume_requested_at timestamptz,
    restore_ms         bigint,
    restore_returned_at timestamptz,
    hello_at           timestamptz,
    hello_ms           bigint,
    error              text NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, seq),
    CONSTRAINT suspension_seq_valid CHECK (seq >= 1)
);
