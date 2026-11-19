-- 0003_attempt_logs.down.sql — 回滚 0003。
-- ROLLBACK DROPS: attempt_logs 的全部尝试明细（含按日分区）与 err_logs.attempt_ordinal 列。
-- 只允许在「旧版本 pod 已全部退出」后执行（见 CHANGELOG 的 down 前置条件）。
SET search_path = public;

DROP TABLE IF EXISTS public.attempt_logs;
-- 序列 OWNED BY attempt_logs.id，随建表 DROP 级联回收；显式再删一次是为了
-- 容忍「表已不在、序列被独立创建过」的中间态（幂等）。
DROP SEQUENCE IF EXISTS public.attempt_logs_id_seq;
ALTER TABLE public.err_logs DROP COLUMN IF EXISTS attempt_ordinal;
