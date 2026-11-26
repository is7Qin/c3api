-- 0004_custom_request_patch.up.sql — 手写迁移（**非生成物**）。
--
-- 为什么手写、为什么不重生成 0001：template_exts.request_patch 是 request_patch
-- 功能（custom 模板出站请求修饰器，spec 2026-11-26-custom-request-patch §4.2）的
-- 持久化列。0001_baseline 一旦发布即不可变（checksum 漂移 ⇒ 启动门 NotReady），
-- 所以 ent schema 新增的列**不重生成 0001**，而是登记到
-- repository.PostBaselineColumns()（baseline 生成器据此裁列），物理列由本迁移
-- ADD COLUMN 建立。
--
-- 为什么是 expand：只加可空列，旧二进制不读新列（回退无需 down）；回滚走 CLI 的
-- -allow-destructive（DROP COLUMN 是破坏性动作）。
--
-- 本列上层承载的 JSON 形状与校验属控制面（domain/service），迁移只管物理列。
SET search_path = public;

ALTER TABLE public.template_exts ADD COLUMN request_patch jsonb NULL;
