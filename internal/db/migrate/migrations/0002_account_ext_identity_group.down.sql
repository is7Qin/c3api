-- ROLLBACK DROPS: 不删表/列；但同一身份已同时存在 Codex 与 BPS 行时，重建两列
-- 全表唯一索引会失败——先人工去重（保留仍要的账号）再回滚，绝不静默删行。
--
-- 先做一次**显式前置检查**：直接让 CREATE INDEX 撞 23505 也能回滚（DDL 在事务
-- 里），但报错只有索引名与重复键，运维看不出「这是合法并存状态，不是脏数据」。
-- 这里提前给出可操作的中文原因，且检查与后续 DDL 在同一事务里——检查通过后
-- 插入的新行仍会被 CREATE INDEX 兜住，不存在检查与执行之间的窗口。
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM public."account_exts"
         WHERE "codex_email" IS NOT NULL
           AND "codex_account_id" IS NOT NULL
         GROUP BY "codex_email", "codex_account_id"
        HAVING count(*) > 1
    ) THEN
        RAISE EXCEPTION 'cannot roll back 0002: the same ChatGPT identity exists as both a Codex and a BPS account; keep one account per identity before rolling back';
    END IF;
END $$;

DROP INDEX IF EXISTS public."accountext_bps_email_account_id";
DROP INDEX IF EXISTS public."accountext_codex_email_codex_account_id";

CREATE UNIQUE INDEX "accountext_codex_email_codex_account_id"
    ON public."account_exts" ("codex_email", "codex_account_id");
