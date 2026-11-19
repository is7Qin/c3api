-- Scope the import idempotency key by provider.
--
-- 0001 的唯一索引是 (codex_email, codex_account_id)：同一个 ChatGPT 账号先在
-- Codex 池里存在（codex-oauth / codex-pat），再通过 BPS 登录加入时，插入 BPS
-- 行会撞这条索引，service 只能报「凭据类型不匹配」——但 BPS 是独立 provider
-- （自己的 endpoint、配额、device profile 与 token store），同一上游账号在两边
-- 各有一行是合法状态，不是冲突。
--
-- 因此把一条全表唯一索引拆成两条**部分**唯一索引：
--   * credential_type <> 'bps'（Codex oauth/pat）仍共用一条键：两者是同一个
--     上游 Codex 账号，第二行仍是脏数据 —— 旧约束的强度一点不降；
--   * credential_type = 'bps' 单独一条键：BPS 自己幂等，同时不再与 Codex 行
--     互相阻塞。
-- 拆分而不是直接把 credential_type 塞进唯一列，是为了避免顺手放宽 Codex 内部
-- 的类型互斥（oauth/pat 混写正是 0001 用两列索引挡住的那类脏数据）。
DROP INDEX IF EXISTS public."accountext_codex_email_codex_account_id";

CREATE UNIQUE INDEX "accountext_codex_email_codex_account_id"
    ON public."account_exts" ("codex_email", "codex_account_id")
    WHERE "credential_type" <> 'bps';

CREATE UNIQUE INDEX "accountext_bps_email_account_id"
    ON public."account_exts" ("codex_email", "codex_account_id")
    WHERE "credential_type" = 'bps';
