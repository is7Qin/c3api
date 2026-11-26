-- 0004_custom_request_patch.down.sql — 回滚 0004。
-- ROLLBACK DROPS: template_exts.request_patch 的全部 custom 请求修饰器配置。
-- 只允许在「旧版本 pod 已全部退出」后执行（见 CHANGELOG 的 down 前置条件）。
SET search_path = public;

ALTER TABLE public.template_exts DROP COLUMN IF EXISTS request_patch;
