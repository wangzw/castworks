-- runcontroller v0.2.0：收件箱事件的种类与订阅完成事件，运行实例的后台作业集合。
-- 列、取值与索引见 docs/milestone/v0.2.0/runcontroller.md“数据”一节。

-- 看守进程最近一次上报的后台作业集合。已有行取空数组。
ALTER TABLE run ADD COLUMN bg_jobs text[] NOT NULL DEFAULT '{}';

-- 收件箱的每一行都是一种事件。已有行都是人类消息；dedup_key 为空串的行
-- 不参与下面的唯一索引，因此人类消息的重复写入行为不变。
ALTER TABLE inbox
    ADD COLUMN kind text NOT NULL DEFAULT 'human_message',
    ADD COLUMN event_id text NOT NULL DEFAULT '',
    ADD COLUMN dedup_key text NOT NULL DEFAULT '',
    ADD COLUMN payload jsonb;

ALTER TABLE inbox DROP CONSTRAINT inbox_text_valid;

-- 人类消息只有 text；订阅完成事件只有 payload，event_id 与 dedup_key 必填。
-- payload 是 SubscriptionCompletion 经 encoding/json 编码的结果。
ALTER TABLE inbox
    ADD CONSTRAINT inbox_kind_valid CHECK (kind IN ('human_message', 'subscription_completed')),
    ADD CONSTRAINT inbox_kind_fields_valid CHECK (
        (kind = 'human_message'
            AND event_id = '' AND dedup_key = '' AND payload IS NULL)
        OR (kind = 'subscription_completed'
            AND event_id <> '' AND dedup_key <> '' AND payload IS NOT NULL)
    ),
    ADD CONSTRAINT inbox_text_valid CHECK (
        (kind = 'human_message' AND octet_length(text) BETWEEN 1 AND 65536)
        OR (kind = 'subscription_completed' AND text = '')
    );

CREATE UNIQUE INDEX inbox_run_dedup_key_unique ON inbox (run_id, dedup_key)
    WHERE dedup_key <> '';
