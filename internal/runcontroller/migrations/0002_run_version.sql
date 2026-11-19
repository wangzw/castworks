-- run 增加 version 列：每次 updateRun 加一。提交返回错误时控制面用新的连接
-- 重读它，判断事务是否其实已经提交。

ALTER TABLE run ADD COLUMN version bigint NOT NULL DEFAULT 0;
