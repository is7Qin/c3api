-- 0003_attempt_logs.up.sql — 手写迁移（**非生成物**）。
--
-- 为什么手写：attempt_logs 是分区父表，被 migrateHookExcludesPartitioned 从 ent
-- 生成的迁移里排除；0001 之后新增的分区父表只能手写。代价是 gen -check 对它
-- **完全无感**（已知盲区，见 .omo/plans/2026-11-18-schema-migrations-spec.md）。
-- 因此建表文本与运行时事实源（repository 的 attemptLogColumnDefs /
-- attemptLogIndexDDLs）由 migrations_ddl_internal_test.go 逐字锚定：改一边不改
-- 另一边，锚测试必红。
--
-- 索引只有 2 条（request_id + created_at、account_id + created_at）——对 spec
-- §3.1「三条索引」的**有意偏离**：分区键 created_at 已能让时间窗查询 pruning，
-- 保留清理走分区退休（整个分区退休，不靠索引），裸 (created_at) 只带来约 1/3 的
-- 写放大与存储，而没有任何查询需要它（S3 的查询恒带 request_id 或 account_id +
-- 时间窗）。
--
-- 本表只记「循环可见的失败尝试」：handled=true 的已交付失败与本地拒绝（401/
-- 额度/402 预检）不产生行，它们的真值仍在 usage_logs / err_logs。
SET search_path = public;

CREATE SEQUENCE public.attempt_logs_id_seq;
CREATE TABLE public.attempt_logs (
	id bigint NOT NULL DEFAULT nextval('public.attempt_logs_id_seq'::regclass),
	request_id varchar NOT NULL,
	attempt_ordinal smallint NOT NULL,
	account_id bigint NOT NULL,
	lane text NOT NULL,
	format text NOT NULL,
	status_code integer NOT NULL DEFAULT 0,
	outcome text NOT NULL,
	error_type text NOT NULL,
	commit_state text NULL,
	error_message text NULL,
	latency_ms bigint NOT NULL DEFAULT 0,
	created_at timestamptz NOT NULL,
	PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
ALTER SEQUENCE public.attempt_logs_id_seq OWNED BY public.attempt_logs.id;
CREATE INDEX attemptlog_request_id_created_at ON public.attempt_logs (request_id, created_at);
CREATE INDEX attemptlog_account_id_created_at ON public.attempt_logs (account_id, created_at);

-- err_logs 的尝试序号（本阶段只加可空列，写入点由 S2b 接）。可空是刻意的：
-- 「循环不可见」的 err_logs 行（本地拒绝 / 已交付失败）没有序号，强制造值就是
-- 编数据。列类型与 attempt_logs.attempt_ordinal 同型，链上排序不跨类型比较。
ALTER TABLE public.err_logs ADD COLUMN attempt_ordinal smallint NULL;
