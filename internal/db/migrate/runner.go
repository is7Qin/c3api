package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/db/migrate/catalog"
)

// 锁键（两参 pg_try_advisory_lock 形式）。单参 `pg_advisory_lock(bigint)` 的键由
// sha256 派生（见 repository/routing.go advisoryLockKey），两参形式的 classid
// 单独占用一个小整数空间，避免与那批哈希键撞车。
const (
	lockClassID int32 = 0x6D67 // "mg"
	lockObjID   int32 = 1
)

// 锁超时与轮询默认值（spec §2.1：默认 60s）。
const (
	defaultLockTimeout = 60 * time.Second
	defaultLockPoll    = 250 * time.Millisecond
)

// LockOptions 是 advisory lock 的行为参数（测试可注入更短超时）。
type LockOptions struct {
	// Timeout 是等待锁的上限；<=0 用默认 60s。
	Timeout time.Duration
	// Poll 是轮询间隔；<=0 用默认 250ms。
	Poll time.Duration
	// ClassID / ObjID 是两参锁键；0 表示用默认常量。
	ClassID  int32
	ObjectID int32
}

func (o LockOptions) withDefaults() LockOptions {
	if o.Timeout <= 0 {
		o.Timeout = defaultLockTimeout
	}
	if o.Poll <= 0 {
		o.Poll = defaultLockPoll
	}
	if o.ClassID == 0 {
		o.ClassID = lockClassID
	}
	if o.ObjectID == 0 {
		o.ObjectID = lockObjID
	}
	return o
}

// LockBusyError 是 try-lock 超时的类型化错误：带 classid/objid/已等待时长。
type LockBusyError struct {
	ClassID  int32
	ObjectID int32
	Waited   time.Duration
}

func (e *LockBusyError) Error() string {
	return fmt.Sprintf("migration lock busy: another instance holds advisory lock (classid=%d, objid=%d); waited %s",
		e.ClassID, e.ObjectID, e.Waited.Round(time.Millisecond))
}

// runner 是一次迁移操作的执行器：db + 迁移文件系统 + 锁参数。
//
// fsys 可注入是为了测试能用 fstest.MapFS 造「一条 expand 迁移」来验 I3，而不必
// 往生产迁移目录里塞测试文件。
type runner struct {
	db   *sql.DB
	fsys fs.FS
	lock LockOptions
}

func newRunner(db *sql.DB, fsys fs.FS, lock LockOptions) *runner {
	return &runner{db: db, fsys: fsys, lock: lock.withDefaults()}
}

// guard 持有专用连接上的 advisory lock。锁必须持在同一条连接上：连接归还池后
// 会话级锁要么还挂着（毒化池），要么随会话结束消失（假持锁）。
type guard struct {
	conn    *sql.Conn
	classID int32
	objID   int32
}

// acquireLock 用 pg_try_advisory_lock(classid, objid) + 轮询等锁，超时报
// LockBusyError（不用阻塞版 pg_advisory_lock：无法给「另一个实例在迁移」一个
// 明确的、可运维的退出路径）。
func acquireLock(ctx context.Context, db *sql.DB, opts LockOptions) (*guard, error) {
	opts = opts.withDefaults()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire dedicated connection: %w", err)
	}
	start := time.Now()
	for {
		var got bool
		if err := conn.QueryRowContext(ctx,
			`SELECT pg_try_advisory_lock($1::int4, $2::int4)`, opts.ClassID, opts.ObjectID).Scan(&got); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("migrate: try advisory lock: %w", err)
		}
		if got {
			return &guard{conn: conn, classID: opts.ClassID, objID: opts.ObjectID}, nil
		}
		waited := time.Since(start)
		if waited >= opts.Timeout {
			_ = conn.Close()
			return nil, &LockBusyError{ClassID: opts.ClassID, ObjectID: opts.ObjectID, Waited: waited}
		}
		timer := time.NewTimer(opts.Poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = conn.Close()
			return nil, fmt.Errorf("migrate: wait advisory lock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// release 解锁并归还连接。解锁语句失败时把底层连接标记为坏（ErrBadConn）让
// database/sql 丢弃它——绝不能让一个还持着锁的会话回到池里。
func (g *guard) release(ctx context.Context) {
	if g == nil || g.conn == nil {
		return
	}
	var unlocked bool
	err := g.conn.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1::int4, $2::int4)`, g.classID, g.objID).Scan(&unlocked)
	if err != nil || !unlocked {
		_ = g.conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = g.conn.Close()
		return
	}
	_ = g.conn.Close()
}

// withGuard 在专用连接持锁的前提下执行 fn；panic 也会走 release（defer）。
func (r *runner) withGuard(ctx context.Context, fn func() error) (err error) {
	g, err := acquireLock(ctx, r.db, r.lock)
	if err != nil {
		return err
	}
	defer g.release(ctx)
	return fn()
}

// up 应用 target 及其之前的全部待应用版本；target <= 0 表示 Head。
//
// contract 闸门：contract 阶段是**不可逆收缩**（DROP TABLE/COLUMN，数据随之丢弃），
// 默认拒跑。判断必须在应用任何一条迁移之前完成，并一次性列出待应用区间内的**全部**
// contract 版本——否则会先跑完 expand、再在 contract 处失败，把库留在半升级状态
// （expand 已提交、contract 未跑），而下一个二进制又只按版本号判断，永远不会重跑。
func (r *runner) up(ctx context.Context, opts UpOptions) (Result, error) {
	migrations, err := loadMigrations(r.fsys)
	if err != nil {
		return Result{}, err
	}
	if len(migrations) == 0 {
		return Result{}, fmt.Errorf("migrate: no migrations found in %s", migrationDir)
	}
	for _, m := range migrations {
		if err := validateSQL(m); err != nil {
			return Result{}, err
		}
	}
	head := migrations[len(migrations)-1].Version
	if opts.Target > head {
		return Result{}, fmt.Errorf("migrate: target %d is beyond this binary's head %d", opts.Target, head)
	}
	if opts.Target <= 0 {
		opts.Target = head
	}
	var res Result
	err = r.withGuard(ctx, func() error {
		// 先读账本（表缺失 = 空账本）：contract 闸门要在任何写入之前拒绝，不能先
		// ensureControl 建表再报错。
		applied, err := loadApplied(ctx, r.db)
		if err != nil {
			return err
		}
		if err := checkApplied(applied, migrations); err != nil {
			return err
		}
		res.From = headOf(applied)
		// 账本空 + 已有 schema ⇒ 拒绝并指向 adopt（spec §2.8：自动收养会把一次
		// 误操作变成一次**静默**误操作）。这里不判断「像不像 baseline」——那是
		// adopt 的逐对象比对职责。
		if res.From == 0 {
			existing, err := existingBusinessObjects(ctx, r.db)
			if err != nil {
				return err
			}
			if existing > 0 {
				return fmt.Errorf("migrate: database already has %d business relations but %s is empty; run -migrate adopt instead of up",
					existing, catalog.ControlTable)
			}
		}
		if !opts.AllowContract {
			if blocked := blockedContracts(migrations, res.From, opts.Target); len(blocked) > 0 {
				return contractGateError(blocked)
			}
		}
		if err := ensureControl(ctx, r.db); err != nil {
			return err
		}
		for _, m := range migrations {
			if m.Version <= res.From || m.Version > opts.Target {
				continue
			}
			row, err := r.apply(ctx, m)
			if err != nil {
				return err
			}
			res.Applied = append(res.Applied, row)
		}
		res.To = res.From
		if n := len(res.Applied); n > 0 {
			res.To = res.Applied[n-1].Version
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// blockedContracts 返回「待应用区间内」的 contract 版本。已应用版本都 ≤ from，
// 所以只需比较版本区间；不存在「先应用 expand、再逐条发现 contract」的渐进路径。
func blockedContracts(migrations []Migration, from, target int64) []Migration {
	var blocked []Migration
	for _, m := range migrations {
		if m.Phase != PhaseContract || m.Version <= from || m.Version > target {
			continue
		}
		blocked = append(blocked, m)
	}
	return blocked
}

// contractGateError 一次性列出全部被挡版本：运维看一次错误就能决定是否显式承认
// 这批不可逆收缩（-migrate-allow-contract）。
func contractGateError(blocked []Migration) error {
	names := make([]string, len(blocked))
	for i, m := range blocked {
		names[i] = fmt.Sprintf("%04d_%s", m.Version, m.Name)
	}
	return fmt.Errorf("migrate: refusing to apply %d contract migration(s) [%s]: contract phases shrink the schema irreversibly (DROP TABLE/COLUMN, data loss); pass -migrate-allow-contract (UpOptions.AllowContract) to confirm",
		len(blocked), strings.Join(names, ", "))
}

// apply 在单事务里执行一条迁移的 DDL 与记账（原子：DDL 失败则连账本一起回滚）。
func (r *runner) apply(ctx context.Context, m Migration) (AppliedMigration, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AppliedMigration{}, fmt.Errorf("migrate: begin tx for %04d_%s: %w", m.Version, m.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	start := time.Now()
	if _, err := tx.ExecContext(ctx, m.UpSQL); err != nil {
		return AppliedMigration{}, fmt.Errorf("migrate: apply %04d_%s: %w", m.Version, m.Name, err)
	}
	execMS := int(time.Since(start).Milliseconds())
	appliedAt := time.Now().UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO public.`+catalog.ControlTable+` (version, name, checksum, phase, origin, applied_at, execution_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		m.Version, m.Name, m.Checksum, m.Phase, OriginExecuted, appliedAt, execMS); err != nil {
		return AppliedMigration{}, fmt.Errorf("migrate: record %04d_%s: %w", m.Version, m.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return AppliedMigration{}, fmt.Errorf("migrate: commit %04d_%s: %w", m.Version, m.Name, err)
	}
	return AppliedMigration{
		Version: m.Version, Name: m.Name, Phase: m.Phase, Origin: OriginExecuted,
		Checksum: m.Checksum, AppliedAt: appliedAt.Format(time.RFC3339), ExecutionMS: execMS,
	}, nil
}

// down 回滚**最后一个**已应用版本。闸门：必须显式 AllowDestructive；
// origin='adopted' 永久禁止。刻意只有一步（没有 steps 参数，也没有循环）：
// 多步回滚把一次误操作放大成多次，生产不该有这条路。
func (r *runner) down(ctx context.Context, opts DownOptions) (DownResult, error) {
	migrations, err := loadMigrations(r.fsys)
	if err != nil {
		return DownResult{}, err
	}
	byVersion := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}
	var res DownResult
	err = r.withGuard(ctx, func() error {
		if err := ensureControl(ctx, r.db); err != nil {
			return err
		}
		applied, err := loadApplied(ctx, r.db)
		if err != nil {
			return err
		}
		if err := checkApplied(applied, migrations); err != nil {
			return err
		}
		sort.Slice(applied, func(i, j int) bool { return applied[i].Version > applied[j].Version })
		if len(applied) == 0 {
			return nil // 空账本：无可回滚，no-op
		}
		row := applied[0]
		m, ok := byVersion[row.Version]
		if !ok {
			return fmt.Errorf("migrate: applied version %04d_%s has no file in this binary", row.Version, row.Name)
		}
		if m.DownSQL == "" {
			return fmt.Errorf("migrate: %04d_%s has no down file", m.Version, m.Name)
		}
		// 先把将要丢弃的东西算出来：拒绝时能让运维看清代价，执行时能打印 SQL。
		if !opts.AllowDestructive {
			return fmt.Errorf("%w; would drop: %s", ErrDestructiveDenied, rollbackSummary(row, m))
		}
		if row.Origin == OriginAdopted {
			return fmt.Errorf("%w: version %04d_%s (origin=adopted)", ErrAdoptedBaseline, row.Version, row.Name)
		}
		if err := r.rollback(ctx, row, m.DownSQL); err != nil {
			return err
		}
		res.Rolled = append(res.Rolled, row)
		res.SQL = append(res.SQL, m.DownSQL)
		return nil
	})
	if err != nil {
		return DownResult{}, err
	}
	return res, nil
}

// rollback 在单事务里执行 down SQL 并删除账本行。
func (r *runner) rollback(ctx context.Context, row AppliedMigration, downSQL string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: begin tx for down %04d_%s: %w", row.Version, row.Name, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, downSQL); err != nil {
		return fmt.Errorf("migrate: rollback %04d_%s: %w", row.Version, row.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM public.`+catalog.ControlTable+` WHERE version = $1`, row.Version); err != nil {
		return fmt.Errorf("migrate: forget %04d_%s: %w", row.Version, row.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: commit down %04d_%s: %w", row.Version, row.Name, err)
	}
	return nil
}

// rollbackSummary 从 down 文件头（`-- ROLLBACK DROPS: …`）取「会丢什么」。
func rollbackSummary(row AppliedMigration, m Migration) string {
	const marker = "ROLLBACK DROPS:"
	for _, line := range strings.Split(m.DownSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if idx := strings.Index(trimmed, marker); idx >= 0 {
			return fmt.Sprintf("%04d_%s (%s)", row.Version, row.Name, strings.TrimSpace(trimmed[idx+len(marker):]))
		}
	}
	return fmt.Sprintf("%04d_%s", row.Version, row.Name)
}

// status 返回账本视图（不写库：账本表缺失视为空账本）。
func (r *runner) status(ctx context.Context) (LedgerStatus, error) {
	migrations, err := loadMigrations(r.fsys)
	if err != nil {
		return LedgerStatus{}, err
	}
	applied, err := loadApplied(ctx, r.db)
	if err != nil {
		return LedgerStatus{}, err
	}
	st := LedgerStatus{Applied: applied}
	byVersion := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}
	seen := make(map[int64]struct{}, len(applied))
	for _, row := range applied {
		seen[row.Version] = struct{}{}
		m, ok := byVersion[row.Version]
		if !ok {
			continue
		}
		if current := m.Checksum; string(current) != string(row.Checksum) {
			st.Mismatches = append(st.Mismatches, ChecksumMismatch{
				Version: row.Version, Name: row.Name,
				Recorded: fmt.Sprintf("%x", row.Checksum), Current: fmt.Sprintf("%x", current),
			})
		}
	}
	for _, m := range migrations {
		if _, ok := seen[m.Version]; !ok {
			st.Pending = append(st.Pending, m)
		}
	}
	return st, nil
}

// verify 计算启动门四态（不写库、不取锁）。
func (r *runner) verify(ctx context.Context) (Verdict, error) {
	migrations, err := loadMigrations(r.fsys)
	if err != nil {
		return Verdict{}, err
	}
	var v Verdict
	for _, m := range migrations {
		v.BinaryHead = m.Version
		if m.Phase == PhaseContract {
			v.BinaryContract = m.Version
		}
	}
	applied, err := loadApplied(ctx, r.db)
	if err != nil {
		return Verdict{}, err
	}
	byVersion := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}
	for _, row := range applied {
		v.DBHead = max(v.DBHead, row.Version)
		if row.Phase == PhaseContract {
			v.DBContract = max(v.DBContract, row.Version)
		}
		m, ok := byVersion[row.Version]
		switch {
		case !ok:
			v.Problems = append(v.Problems, fmt.Sprintf("applied version %04d_%s is not in this binary", row.Version, row.Name))
		case string(m.Checksum) != string(row.Checksum):
			v.Problems = append(v.Problems, fmt.Sprintf("checksum mismatch for %04d_%s: recorded=%x current=%x",
				row.Version, row.Name, row.Checksum, m.Checksum))
		}
	}
	if v.DBContract > v.BinaryContract {
		v.Problems = append(v.Problems, fmt.Sprintf("database contract head %d is ahead of binary contract head %d (contract migration already applied)", v.DBContract, v.BinaryContract))
	}
	// db_head > binary_head 不是问题：expand-only 保证旧二进制能跑新库（k8s 回退的关键）。
	return v, nil
}

// adopt 把「账本空 + 已有 schema」的库认成已知发布版本的 baseline。
func (r *runner) adopt(ctx context.Context, opts AdoptOptions) (AdoptResult, error) {
	if opts.Snapshot == "" {
		return AdoptResult{}, fmt.Errorf("migrate: adopt requires an explicit snapshot tag")
	}
	target, _, ok := parseStem(opts.Snapshot + "_x")
	if !ok {
		return AdoptResult{}, fmt.Errorf("migrate: adopt snapshot tag %q is not a version (want \"0001\")", opts.Snapshot)
	}
	want, err := Snapshot(opts.Snapshot)
	if err != nil {
		return AdoptResult{}, err
	}
	if want.Static == "" {
		return AdoptResult{}, fmt.Errorf("migrate: snapshot %q is empty", opts.Snapshot)
	}
	migrations, err := loadMigrations(r.fsys)
	if err != nil {
		return AdoptResult{}, err
	}
	var res AdoptResult
	err = r.withGuard(ctx, func() error {
		if err := ensureControl(ctx, r.db); err != nil {
			return err
		}
		applied, err := loadApplied(ctx, r.db)
		if err != nil {
			return err
		}
		if len(applied) > 0 {
			return fmt.Errorf("migrate: ledger is not empty (%d applied versions); adopt is only for a fresh ledger", len(applied))
		}
		got, err := catalog.Dump(ctx, r.db)
		if err != nil {
			return err
		}
		// ① 逐对象相等（缺失 + 多出都要报，收养不接受「差不多了」）。白名单只
		// 放宽「多出的行」，且命中项必须回显。
		tolerated, diff := splitStaticDiff(want, got, opts.Tolerate)
		if len(diff) > 0 {
			return fmt.Errorf("migrate: catalog does not match snapshot %s:\n%s", opts.Snapshot, strings.Join(diff, "\n"))
		}
		res.Tolerated = tolerated
		// ② 账本丢失探测：账本空但 settings.schema_version 有值 ⇒ 拒绝。
		stored, hasRow, err := settingsSchemaVersion(ctx, r.db)
		if err != nil {
			return err
		}
		if hasRow && stored != "" && stored != opts.SchemaVersion {
			return fmt.Errorf("migrate: ledger lost: settings.schema_version=%q while %s is empty (pass the expected value to adopt deliberately)",
				stored, catalog.ControlTable)
		}
		// ③ 反向哨兵：库里的业务表必须全部属于 baseline（防共用库误收养）。
		known := make(map[string]struct{})
		for _, line := range want.Lines() {
			if fields := strings.Split(line, "|"); fields[0] == "table" {
				known[fields[1]] = struct{}{}
			}
		}
		for _, line := range got.Lines() {
			fields := strings.Split(line, "|")
			if fields[0] != "table" {
				continue
			}
			if _, ok := known[fields[1]]; !ok {
				return fmt.Errorf("migrate: table %s exists but is not part of snapshot %s (shared database?); refusing to adopt", fields[1], opts.Snapshot)
			}
		}
		now := time.Now().UTC()
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migrate: begin adopt tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		for _, m := range migrations {
			if m.Version > target {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO public.`+catalog.ControlTable+` (version, name, checksum, phase, origin, applied_at, execution_ms)
				 VALUES ($1, $2, $3, $4, $5, $6, 0)`,
				m.Version, m.Name, m.Checksum, m.Phase, OriginAdopted, now); err != nil {
				return fmt.Errorf("migrate: adopt %04d_%s: %w", m.Version, m.Name, err)
			}
			res.Adopted = append(res.Adopted, AppliedMigration{
				Version: m.Version, Name: m.Name, Phase: m.Phase, Origin: OriginAdopted,
				Checksum: m.Checksum, AppliedAt: now.Format(time.RFC3339),
			})
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate: commit adopt tx: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdoptResult{}, err
	}
	return res, nil
}

// splitStaticDiff 把 want → got 的静态差异分成「命中白名单的多出行」与「真差异」。
//
// 白名单是**整行字面量**（`==`），粒度 = 一个完整对象：同名约束只要 def 不同就是
// 另一行，前缀/正则式匹配在这里不成立。缺失行永不被白名单覆盖（那说明库不是这个
// 版本）。
func splitStaticDiff(want, got catalog.Manifest, tolerate []string) (tolerated, diff []string) {
	allowed := make(map[string]struct{}, len(tolerate))
	for _, line := range tolerate {
		allowed[line] = struct{}{}
	}
	for _, line := range catalog.Missing(want, got) {
		diff = append(diff, "missing: "+line)
	}
	have := make(map[string]struct{}, len(want.Lines()))
	for _, line := range want.Lines() {
		have[line] = struct{}{}
	}
	for _, line := range got.Lines() {
		if _, ok := have[line]; ok {
			continue
		}
		if _, ok := allowed[line]; ok {
			tolerated = append(tolerated, line)
			continue
		}
		diff = append(diff, "unexpected: "+line)
	}
	sort.Strings(tolerated)
	sort.Strings(diff)
	const limit = 20
	if len(diff) > limit {
		diff = append(diff[:limit], fmt.Sprintf("… (%d more)", len(diff)-limit))
	}
	return tolerated, diff
}

// settingsSchemaVersion 读 settings 表里的 schema_version 行；表不存在时视为无行。
func settingsSchemaVersion(ctx context.Context, db *sql.DB) (value string, hasRow bool, err error) {
	rows, err := db.QueryContext(ctx, `SELECT value FROM public.settings WHERE key = 'schema_version' LIMIT 1`)
	if err != nil {
		if isMissingRelation(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("migrate: read settings.schema_version: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	if err := rows.Scan(&value); err != nil {
		return "", false, fmt.Errorf("migrate: scan settings.schema_version: %w", err)
	}
	return value, true, nil
}

// checkApplied 校验已应用版本与本二进制内嵌文件逐条对齐：未知版本与 checksum
// 漂移都是硬错误（迁移不可变）。
func checkApplied(applied []AppliedMigration, migrations []Migration) error {
	byVersion := make(map[int64]Migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}
	sorted := append([]AppliedMigration(nil), applied...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	for _, row := range sorted {
		m, ok := byVersion[row.Version]
		if !ok {
			return fmt.Errorf("migrate: applied version %04d_%s is not in this binary", row.Version, row.Name)
		}
		if string(m.Checksum) != string(row.Checksum) {
			return fmt.Errorf("migrate: checksum mismatch for %04d_%s: recorded=%x current=%x (migrations are immutable; add a new version instead)",
				row.Version, row.Name, row.Checksum, m.Checksum)
		}
	}
	return nil
}

// existingBusinessObjects 数一下库里已有的业务关系（表/分区父表），账本表本身不算。
func existingBusinessObjects(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname <> $1`,
		catalog.ControlTable).Scan(&n); err != nil {
		return 0, fmt.Errorf("migrate: inspect existing schema: %w", err)
	}
	return n, nil
}

// loadApplied 读账本（表缺失 = 空账本）。
func loadApplied(ctx context.Context, db *sql.DB) ([]AppliedMigration, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT version, name, checksum, phase, origin, applied_at, execution_ms
		 FROM public.`+catalog.ControlTable+` ORDER BY version`)
	if err != nil {
		if isMissingRelation(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("migrate: read %s: %w", catalog.ControlTable, err)
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var (
			row       AppliedMigration
			appliedAt time.Time
		)
		if err := rows.Scan(&row.Version, &row.Name, &row.Checksum, &row.Phase, &row.Origin, &appliedAt, &row.ExecutionMS); err != nil {
			return nil, fmt.Errorf("migrate: scan %s: %w", catalog.ControlTable, err)
		}
		row.AppliedAt = appliedAt.UTC().Format(time.RFC3339)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: iterate %s: %w", catalog.ControlTable, err)
	}
	return out, nil
}

func headOf(applied []AppliedMigration) int64 {
	var head int64
	for _, row := range applied {
		head = max(head, row.Version)
	}
	return head
}
