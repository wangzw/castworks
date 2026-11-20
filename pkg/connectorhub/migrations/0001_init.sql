-- connectorhub 的持久化表结构。列见 docs/milestone/v0.2.0/connectorhub.md
-- “数据”一节；完成后投递记录里的 event 是待写入收件箱的订阅完成事件。

CREATE TABLE connector_event (
    append_seq  bigint PRIMARY KEY,
    system      text NOT NULL,
    delivery_id text NOT NULL,
    event_type  text NOT NULL,
    project_ids text[] NOT NULL DEFAULT '{}',
    correlation jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL,
    payload     bytea NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT connector_event_append_seq_valid CHECK (append_seq >= 1),
    CONSTRAINT connector_event_system_delivery_id_key UNIQUE (system, delivery_id)
);

-- 回溯匹配按关联键的子集关系定位候选事件。
CREATE INDEX connector_event_correlation_gin ON connector_event USING gin (correlation);

-- 追加序号由这唯一一行分配。id 只用来把表限制为单行。
CREATE TABLE connector_event_counter (
    id       boolean PRIMARY KEY DEFAULT true,
    next_seq bigint NOT NULL,
    CONSTRAINT connector_event_counter_single_row CHECK (id),
    CONSTRAINT connector_event_counter_next_seq_valid CHECK (next_seq >= 1)
);

INSERT INTO connector_event_counter (id, next_seq) VALUES (true, 1);

CREATE TABLE subscription (
    sub_id       text PRIMARY KEY,
    run_id       text NOT NULL,
    project_id   text NOT NULL,
    system       text NOT NULL,
    event        text NOT NULL,
    correlation  jsonb NOT NULL DEFAULT '{}'::jsonb,
    status       text NOT NULL,
    terminal     text NOT NULL DEFAULT '',
    since_cursor bigint NOT NULL,
    dedup_spec   jsonb NOT NULL,
    created_at   timestamptz NOT NULL,
    completed_at timestamptz,
    CONSTRAINT subscription_status_valid CHECK (status IN ('active', 'completed', 'cancelled')),
    CONSTRAINT subscription_since_cursor_valid CHECK (since_cursor >= 1)
);

-- 取消扫描按运行实例查活动订阅。
CREATE INDEX subscription_active_run ON subscription (run_id) WHERE status = 'active';

CREATE TABLE subscription_claim (
    sub_id      text NOT NULL,
    delivery_id text NOT NULL,
    PRIMARY KEY (sub_id, delivery_id)
);

CREATE TABLE completion_delivery (
    sub_id       text NOT NULL,
    run_id       text NOT NULL,
    event_id     text NOT NULL,
    dedup_key    text NOT NULL,
    event        jsonb NOT NULL,
    status       text NOT NULL DEFAULT 'pending',
    attempts     integer NOT NULL DEFAULT 0,
    last_error   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    CONSTRAINT completion_delivery_sub_id_dedup_key_key UNIQUE (sub_id, dedup_key),
    CONSTRAINT completion_delivery_status_valid CHECK (status IN ('pending', 'delivered', 'rejected')),
    CONSTRAINT completion_delivery_attempts_valid CHECK (attempts >= 0)
);

-- 后台扫描重投全部 pending 记录。
CREATE INDEX completion_delivery_pending ON completion_delivery (created_at) WHERE status = 'pending';
