// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// fakeStore 以值语义模拟真实仓库（ent 每次返回新对象、无指针别名）：
// Create/Get/Update 均返回副本，存库条目一经写入不再被外部指针修改。
// 若直接存/返回调用方指针，原地修改会透过别名污染测试持有的旧引用
// （评审发现：测试必然失败或退化为恒真断言）。
// 实现 service.Store 接口，供 handler 测试使用。
type fakeStore struct {
	mu          sync.Mutex
	tpls        map[int64]*domain.Template
	accs        map[int64]*domain.Account
	groups      map[int64]*domain.Group
	accGroups   map[int64][]int64 // accountID → groupIDs（账号侧绑定，Set/GetAccountGroups）
	keys        map[int64]*domain.Key
	users       map[int64]*domain.User
	settings    map[string]*domain.Setting
	rules       map[int64]domain.Rule
	logs        []*domain.UsageLog
	stats       []*domain.StatBucket
	entityStats []*domain.EntityStatBucket
	assign      map[int64][]int64 // groupID → 授予 user_id 列表（group_assignments 模拟）
	assignMult  map[[2]int64]*int // (groupID, userID) → 专属价格倍率（nil = 未设置；T3.5 按组）
	codes       map[int64]*domain.RedemptionCode
	uses        map[int64]*domain.RedemptionUse
	temps       []*fakeTempRow // 临时额度行（domain 无 TempBalance 类型，标量参数即全字段）
	// pricings 模型价格（key = model，一行 = 最终生效价；manual > litellm 优先级
	// 语义与真实仓库一致）。
	pricings map[string]*domain.PriceEntry
	// imagePrices 图片生成价格（Task A 双线；同 pricings 优先级语义）。
	imagePrices map[string]*domain.PriceEntry
	// functionPrices 按单元计费功能类价格（价格表三件套；同 pricings 优先级语义）。
	functionPrices map[string]*domain.PriceEntry
	priceEntries   map[string]*domain.PriceEntry
	priceVariants  map[string][]*domain.PriceVariant
	// tplExts/accExts 模板/账号类型化扩展（key = 父 id，镜像仓库 1:1 唯一索引）。
	tplExts        map[int64]*domain.TemplateExt
	accExts        map[int64]*domain.AccountExt
	emailTemplates map[string]*domain.EmailTemplate
	emailCodes     map[string]*domain.EmailCode
	nextID         int64
	// lastPatch 记录最近一次 UpdateAccountsBatch 收到的 patch（评审 M3：
	// 断言 handler 的 group_ids nil/[] 映射是否真正传到了 repo 层）。
	lastPatch repository.AccountPatch
	// aggIDs/aggFrom/aggTo 记录最近一次 ScanUsageAgg 收参（缺省时间注入断言用）。
	aggIDs  []int64
	aggFrom time.Time
	aggTo   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tpls: make(map[int64]*domain.Template), accs: make(map[int64]*domain.Account),
		groups: make(map[int64]*domain.Group), accGroups: make(map[int64][]int64),
		keys: make(map[int64]*domain.Key), users: make(map[int64]*domain.User),
		settings: make(map[string]*domain.Setting), rules: make(map[int64]domain.Rule),
		assign: make(map[int64][]int64), assignMult: make(map[[2]int64]*int),
		codes: make(map[int64]*domain.RedemptionCode),
		uses:  make(map[int64]*domain.RedemptionUse), pricings: make(map[string]*domain.PriceEntry),
		imagePrices:    make(map[string]*domain.PriceEntry),
		functionPrices: make(map[string]*domain.PriceEntry),
		priceEntries:   make(map[string]*domain.PriceEntry),
		priceVariants:  make(map[string][]*domain.PriceVariant),
		tplExts:        make(map[int64]*domain.TemplateExt), accExts: make(map[int64]*domain.AccountExt),
		emailTemplates: make(map[string]*domain.EmailTemplate),
		emailCodes:     make(map[string]*domain.EmailCode),
		nextID:         1,
	}
}

// DeleteKeysByGroup 满足 KeyStore（组删除前置清理；返回被删明文列表）。
// 镜像真实 repo 原子 SQL 语义：只软删未删 key（deleted_at IS NULL）并返回其
// 明文；已软删 key 不动（其明文此前已从 Auth 移除）。
func (f *fakeStore) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var raws []string
	for _, k := range f.keys {
		if k.GroupID == groupID && k.DeletedAt == nil {
			now := time.Now()
			k.DeletedAt = &now
			raws = append(raws, k.KeyRaw)
		}
	}
	return raws, nil
}

var _ service.Store = (*fakeStore)(nil)

// missingErr 模拟真实 repo 单资源缺 id 错误（与批量 fake 同格式：
// repository.ErrNotFound 包装，service mapRepoErr 据此映射 404 含 id）。
func missingErr(id int64) error {
	return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
}

func (f *fakeStore) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.templateNameConflictLocked(0, t.Name); err != nil {
		return nil, err
	}
	t.ID = f.nextID
	f.nextID++
	c := *t
	f.tpls[t.ID] = &c
	return t, nil
}

func (f *fakeStore) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tpls[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *t
	return &c, nil
}

// GetTemplatesByIDs 批量取模板（镜像真实 repo：id IN；缺失 id 不报错——
// 数量 < 请求数由调用方对比）。
func (f *fakeStore) GetTemplatesByIDs(ctx context.Context, ids []int64) ([]*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Template, 0, len(ids))
	for _, id := range ids {
		if t, ok := f.tpls[id]; ok {
			c := *t
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeStore) ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Template, 0, len(f.tpls))
	for _, t := range f.tpls {
		c := *t
		out = append(out, &c)
	}
	return paginate(out, q), int64(len(out)), nil
}

func (f *fakeStore) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.templateNameConflictLocked(t.ID, t.Name); err != nil {
		return nil, err
	}
	c := *t
	f.tpls[t.ID] = &c
	return &c, nil
}

func (f *fakeStore) DeleteTemplate(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tpls[id]; !ok {
		return missingErr(id)
	}
	delete(f.tpls, id)
	return nil
}

func (f *fakeStore) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.ID = f.nextID
	f.nextID++
	c := *a
	f.accs[a.ID] = &c
	return a, nil
}

func (f *fakeStore) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accs[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *a
	return &c, nil
}

func (f *fakeStore) ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Account, 0, len(f.accs))
	for _, a := range f.accs {
		c := *a
		out = append(out, &c)
	}
	return paginate(out, q), int64(len(out)), nil
}

func (f *fakeStore) UpdateAccount(ctx context.Context, a *domain.Account, cooldownUntil *time.Time) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *a
	if cooldownUntil != nil {
		c.CooldownUntil = cooldownUntil
	}
	f.accs[a.ID] = &c
	return &c, nil
}

func (f *fakeStore) DeleteAccount(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accs[id]; !ok {
		return missingErr(id)
	}
	delete(f.accs, id)
	return nil
}

func (f *fakeStore) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.groupNameConflictLocked(0, g.Name); err != nil {
		return nil, err
	}
	g.ID = f.nextID
	f.nextID++
	c := *g
	f.groups[g.ID] = &c
	return g, nil
}

func (f *fakeStore) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *g
	return &c, nil
}

func (f *fakeStore) ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Group, 0, len(f.groups))
	for _, g := range f.groups {
		if g.DeletedAt != nil {
			continue // 软删过滤（真实 repo 同谓词）
		}
		c := *g
		out = append(out, &c)
	}
	return paginate(out, q), int64(len(out)), nil
}

func (f *fakeStore) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.groupNameConflictLocked(g.ID, g.Name); err != nil {
		return nil, err
	}
	c := *g
	f.groups[g.ID] = &c
	return &c, nil
}

// DeleteGroup 软删语义（镜像真实 repo：行保留 + deleted_at 置值；GET 单个仍
// 可查已删项，列表过滤由 ListGroups 做）。
func (f *fakeStore) DeleteGroup(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[id]
	if !ok {
		return missingErr(id)
	}
	now := time.Now()
	g.DeletedAt = &now
	return nil
}

func (f *fakeStore) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accGroups[accountID] = slices.Clone(groupIDs)
	return nil
}

func (f *fakeStore) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accGroups[accountID]), nil
}

// LoadGroupAccounts 单组账号（F1 删组校验用；镜像真实 repo：已删账号过滤）。
func (f *fakeStore) LoadGroupAccounts(ctx context.Context, groupID int64) ([]*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.Account
	for id, gids := range f.accGroups {
		if !slices.Contains(gids, groupID) {
			continue
		}
		a, ok := f.accs[id]
		if !ok || a.DeletedAt != nil {
			continue
		}
		c := *a
		out = append(out, &c)
	}
	return out, nil
}

// --- 模板/账号类型化扩展（TemplateExtStore / AccountExtStore） ---

// UpsertTemplateExt 幂等写入（镜像真实 repo 1:1 upsert 语义：已存在 → 全列
// 替换含 NULL 清空；缺失 → 插入）。
func (f *fakeStore) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *e
	f.tplExts[e.TemplateID] = &c
	return &c, nil
}

func (f *fakeStore) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.tplExts[templateID]
	if !ok {
		return nil, fmt.Errorf("%w: template_id=%d missing", repository.ErrNotFound, templateID)
	}
	c := *e
	return &c, nil
}

func (f *fakeStore) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *e
	f.accExts[e.AccountID] = &c
	return &c, nil
}

// TryInsertAccountExt 先写者胜空插入（镜像真实 repo ON CONFLICT DO NOTHING
// 语义：已存在 → 跳过不覆盖返回 false；缺失 → 插入返回 true）。
func (f *fakeStore) TryInsertAccountExt(ctx context.Context, e *domain.AccountExt) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accExts[e.AccountID]; ok {
		return false, nil
	}
	c := *e
	f.accExts[e.AccountID] = &c
	return true, nil
}

func (f *fakeStore) GetAccountExt(ctx context.Context, accountID int64) (*domain.AccountExt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.accExts[accountID]
	if !ok {
		return nil, fmt.Errorf("%w: account_id=%d missing", repository.ErrNotFound, accountID)
	}
	c := *e
	return &c, nil
}

// FindAccountExtByCodexKey 组合幂等键查重（Task B 批量导入；镜像真实 repo
// 双条件 AND——缺行 → ErrNotFound）。
func (f *fakeStore) FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string) (*domain.AccountExt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.accExts {
		if e.CodexEmail != nil && *e.CodexEmail == codexEmail &&
			e.CodexAccountID != nil && *e.CodexAccountID == codexAccountID {
			c := *e
			return &c, nil
		}
	}
	return nil, fmt.Errorf("%w: codex_email=%q codex_account_id=%q missing", repository.ErrNotFound, codexEmail, codexAccountID)
}

// WriteOAuthRotation oauth 凭据三列部分更新（镜像真实 repo 部分更新语义——
// 其余列零触碰）；行缺失 → ErrNotFound。
func (f *fakeStore) WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.accExts[accountID]
	if !ok {
		return fmt.Errorf("%w: account_id=%d ext row missing", repository.ErrNotFound, accountID)
	}
	e.CodexOAuthToken = &at
	e.CodexOAuthRefreshToken = &rt
	e.CodexOAuthExpiresAt = expiresAt
	return nil
}

// WritePATKey pat 凭据列部分更新（WriteOAuthRotation 的 pat 对称形态）；
// 行缺失 → ErrNotFound。
func (f *fakeStore) WritePATKey(ctx context.Context, accountID int64, patKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.accExts[accountID]
	if !ok {
		return fmt.Errorf("%w: account_id=%d ext row missing", repository.ErrNotFound, accountID)
	}
	e.CodexPATKey = &patKey
	return nil
}

// QueryUsages 模拟 repo 过滤（R4-M2 防假绿：完整过滤面与真实 repo
// QueryUsages 逐项一致——归属四元组/model/error_type/时间范围 + cursor 游标
// （id < cursor）+ ID 降序 + limit+1 探测；user_id > 0 强制过滤——
// /api/user/usage_logs 防越权测试依赖此语义）。去 Total（契约已移除）。
func (f *fakeStore) QueryUsages(ctx context.Context, q repository.UsageQuery) ([]*domain.UsageLog, error) {
	return f.queryLogs(logFilter{
		groupID: q.GroupID, accountID: q.AccountID, userID: q.UserID, keyID: q.KeyID,
		model: q.Model, format: q.Format, errorType: q.ErrorType,
		from: q.From, to: q.To, cursor: q.Cursor, limit: q.Limit,
	}), nil
}

// QueryErrLogs 模拟 repo 过滤（/err_logs：usage_logs 过滤面 + status_code 专属
// 列；user_id > 0 强制过滤——/api/user/err_logs 防越权测试依赖此语义）。
func (f *fakeStore) QueryErrLogs(ctx context.Context, q repository.ErrLogQuery) ([]*domain.UsageLog, error) {
	return f.queryLogs(logFilter{
		groupID: q.GroupID, accountID: q.AccountID, userID: q.UserID, keyID: q.KeyID,
		model: q.Model, format: q.Format, errorType: q.ErrorType, statusCode: q.StatusCode,
		from: q.From, to: q.To, cursor: q.Cursor, limit: q.Limit,
	}), nil
}

// ScanUsageAgg 模拟 repo 聚合（/api/admin/accounts/usage 查询面——与真实 SQL 同
// 语义：account_ids 过滤 + created_at 半开区间 [from, to)；SUM/COUNT 毫分原样
// 聚合；无记录账号无键）。aggFrom/aggTo/aggIDs 记录最近一次收参（缺省时间
// 注入断言用）。
func (f *fakeStore) ScanUsageAgg(ctx context.Context, accountIDs []int64, from, to time.Time) (map[int64]*domain.UsageAgg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aggIDs = append([]int64(nil), accountIDs...)
	f.aggFrom, f.aggTo = from, to
	inSet := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		inSet[id] = struct{}{}
	}
	out := make(map[int64]*domain.UsageAgg)
	for _, l := range f.logs {
		if _, ok := inSet[l.AccountID]; !ok {
			continue
		}
		if l.CreatedAt.Before(from) || !l.CreatedAt.Before(to) {
			continue
		}
		a := out[l.AccountID]
		if a == nil {
			a = &domain.UsageAgg{AccountID: l.AccountID}
			out[l.AccountID] = a
		}
		a.Requests++
		a.Cost += l.Cost
		a.RawCost += l.RawCost
		a.TotalTokens += l.TotalTokens
	}
	return out, nil
}

// logFilter 日志查询过滤面（fake 内部表示——镜像真实 repo UsageQuery/
// ErrLogQuery 语义逐项：0/空 = 不过滤；StatusCode 仅 err_logs 有）。
type logFilter struct {
	groupID, accountID, userID, keyID int64
	model, format, errorType          string
	statusCode                        int
	from, to                          *time.Time
	cursor                            int64
	limit                             int
}

// queryLogs 过滤 + cursor 游标（id < cursor）+ ID 降序 + limit+1 探测
// （与真实 repo QueryUsages/QueryErrLogs keyset 语义一致；Limit ≤ 0 缺省 20）。
// 返回副本防别名污染。
func (f *fakeStore) queryLogs(q logFilter) []*domain.UsageLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []*domain.UsageLog
	for _, l := range f.logs {
		if q.cursor > 0 && l.ID >= q.cursor {
			continue
		}
		if q.groupID > 0 && l.GroupID != q.groupID {
			continue
		}
		if q.accountID > 0 && l.AccountID != q.accountID {
			continue
		}
		if q.userID > 0 && l.UserID != q.userID {
			continue
		}
		if q.keyID > 0 && l.KeyID != q.keyID {
			continue
		}
		if q.model != "" && l.Model != q.model {
			continue
		}
		if q.format != "" && string(l.Format) != q.format {
			continue
		}
		if q.errorType != "" && string(l.ErrorType) != q.errorType {
			continue
		}
		if q.statusCode > 0 && l.StatusCode != q.statusCode {
			continue
		}
		if q.from != nil && l.CreatedAt.Before(*q.from) {
			continue
		}
		if q.to != nil && l.CreatedAt.After(*q.to) {
			continue
		}
		c := *l
		matched = append(matched, &c)
	}
	if q.limit <= 0 {
		q.limit = 20
	}
	// ID 降序（与真实 repo Order(ent.Desc(FieldID)) 一致）
	slices.SortFunc(matched, func(a, b *domain.UsageLog) int { return cmp.Compare(b.ID, a.ID) })
	if len(matched) > q.limit+1 {
		matched = matched[:q.limit+1]
	}
	return matched
}

// --- 批量操作（模拟 repo 语义：事务内存在性检查，缺失 id 返回
// repository.ErrNotFound 包装错误含 id；更新仅应用 patch 非 nil 字段） ---

// missingID 返回 ids 中第一个不存在条目的 id（与真实 repo 的
// checkXxxExist 差集语义一致）。
func (f *fakeStore) missingID(ids []int64, exists func(id int64) bool) (int64, bool) {
	for _, id := range ids {
		if !exists(id) {
			return id, true
		}
	}
	return 0, false
}

func (f *fakeStore) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.tpls[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.tpls, id)
	}
	return nil
}

func (f *fakeStore) UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.tpls[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		t := f.tpls[id]
		if p.Name != nil {
			t.Name = *p.Name
		}
		if p.BaseURL != nil {
			t.BaseURL = *p.BaseURL
		}
		if p.SupportedFormats != nil {
			t.SupportedFormats = *p.SupportedFormats
		}
		if p.Models != nil {
			t.Models = *p.Models
		}
		if p.FormatModels != nil {
			t.FormatModels = *p.FormatModels
		}
		if p.ModelMapping != nil {
			m := make(map[string]string, len(*p.ModelMapping))
			for k, v := range *p.ModelMapping {
				m[k] = v
			}
			t.ModelMapping = m
		}
	}
	return nil
}

func (f *fakeStore) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.accs[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.accs, id)
	}
	return nil
}

func (f *fakeStore) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.accs[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	// 组存在性（与真实 repo 的 checkGroupExist 同级语义：非空 group_ids 全查）
	if p.GroupIDs != nil {
		for _, gid := range *p.GroupIDs {
			if _, ok := f.groups[gid]; !ok {
				return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, gid)
			}
		}
	}
	f.lastPatch = p
	for _, id := range ids {
		a := f.accs[id]
		if p.Name != nil {
			a.Name = *p.Name
		}
		if p.TemplateID != nil {
			a.TemplateID = *p.TemplateID
		}
		if p.UpstreamKey != nil {
			a.UpstreamKey = *p.UpstreamKey
		}
		if p.BaseURL != nil {
			// 批量三态（C1，对齐真实 repo）："" = 清空（NULL = 继承模板）；非空 = 落值
			if *p.BaseURL == "" {
				a.BaseURL = nil
			} else {
				b := *p.BaseURL
				a.BaseURL = &b
			}
		}
		if p.Status != nil {
			a.Status = *p.Status
		}
		if p.Weight != nil {
			a.Weight = *p.Weight
		}
		if p.MaxConcurrency != nil {
			a.MaxConcurrency = *p.MaxConcurrency
		}
		if p.GroupIDs != nil {
			f.accGroups[id] = slices.Clone(*p.GroupIDs)
		}
		if p.CooldownUntil != nil {
			a.CooldownUntil = p.CooldownUntil
		}
	}
	return nil
}

func (f *fakeStore) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.groups[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		now := time.Now()
		f.groups[id].DeletedAt = &now
	}
	return nil
}

func (f *fakeStore) UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.groups[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		g := f.groups[id]
		if p.Name != nil {
			g.Name = *p.Name
		}
	}
	return nil
}

func (f *fakeStore) StatsTrend(ctx context.Context, from, to time.Time, unit string, groupID int64, model string) ([]*domain.StatBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[time.Time]*domain.StatBucket{}
	for _, b := range f.stats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if groupID > 0 && b.GroupID != groupID {
			continue
		}
		if model != "" && b.Model != model {
			continue
		}
		var bt time.Time
		if unit == "hour" {
			bt = b.BucketTime.Truncate(time.Hour)
		} else {
			bt = b.BucketTime.UTC().Truncate(24 * time.Hour)
		}
		v, ok := m[bt]
		if !ok {
			v = &domain.StatBucket{BucketTime: bt, Model: b.Model, GroupID: b.GroupID}
			if unit == "day" {
				v.Model = model
				if model == "" {
					v.Model = ""
				}
			}
			m[bt] = v
		}
		v.RequestCount += b.RequestCount
		v.ErrorCount += b.ErrorCount
		v.CallCount += b.CallCount
		v.InputTokens += b.InputTokens
		v.OutputTokens += b.OutputTokens
		v.TotalTokens += b.TotalTokens
		v.CacheReadTokens += b.CacheReadTokens
		v.CacheCreationTokens += b.CacheCreationTokens
		v.Cost += b.Cost
		v.RawCost += b.RawCost
		v.TTFTTotalMS += b.TTFTTotalMS
		v.TTFTCount += b.TTFTCount
		if b.TTFTMaxMS > v.TTFTMaxMS {
			v.TTFTMaxMS = b.TTFTMaxMS
		}
	}
	out := make([]*domain.StatBucket, 0, len(m))
	for _, v := range m {
		c := *v
		out = append(out, &c)
	}
	slices.SortFunc(out, func(a, b *domain.StatBucket) int { return a.BucketTime.Compare(b.BucketTime) })
	return out, nil
}

func (f *fakeStore) StatsTop(ctx context.Context, from, to time.Time, entityType string, by string, limit int) ([]*domain.EntityStatBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	agg := map[int64]*domain.EntityStatBucket{}
	for _, b := range f.entityStats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if b.EntityType != entityType {
			continue
		}
		v, ok := agg[b.EntityID]
		if !ok {
			v = &domain.EntityStatBucket{EntityType: entityType, EntityID: b.EntityID, Model: ""}
			agg[b.EntityID] = v
		}
		v.RequestCount += b.RequestCount
		v.ErrorCount += b.ErrorCount
		v.CallCount += b.CallCount
		v.InputTokens += b.InputTokens
		v.OutputTokens += b.OutputTokens
		v.TotalTokens += b.TotalTokens
		v.CacheReadTokens += b.CacheReadTokens
		v.CacheCreationTokens += b.CacheCreationTokens
		v.Cost += b.Cost
		v.RawCost += b.RawCost
		v.TTFTTotalMS += b.TTFTTotalMS
		v.TTFTCount += b.TTFTCount
		if b.TTFTMaxMS > v.TTFTMaxMS {
			v.TTFTMaxMS = b.TTFTMaxMS
		}
	}
	out := make([]*domain.EntityStatBucket, 0, len(agg))
	for _, v := range agg {
		c := *v
		out = append(out, &c)
	}
	slices.SortFunc(out, func(a, b *domain.EntityStatBucket) int {
		var va, vb int64
		switch by {
		case "cost":
			va, vb = a.Cost, b.Cost
		case "requests":
			va, vb = a.RequestCount, b.RequestCount
		case "tokens":
			va, vb = a.TotalTokens, b.TotalTokens
		default:
			va, vb = a.Cost, b.Cost
		}
		if va != vb {
			return cmp.Compare(vb, va)
		}
		return cmp.Compare(a.EntityID, b.EntityID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) StatsEntityTrend(ctx context.Context, from, to time.Time, unit string, entityType string, entityID int64, model string) ([]*domain.EntityStatBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[time.Time]*domain.EntityStatBucket{}
	for _, b := range f.entityStats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if b.EntityType != entityType || b.EntityID != entityID {
			continue
		}
		if model != "" && b.Model != model {
			continue
		}
		var bt time.Time
		if unit == "hour" {
			bt = b.BucketTime.Truncate(time.Hour)
		} else {
			bt = b.BucketTime.UTC().Truncate(24 * time.Hour)
		}
		v, ok := m[bt]
		if !ok {
			v = &domain.EntityStatBucket{BucketTime: bt, EntityType: entityType, EntityID: entityID, Model: b.Model}
			m[bt] = v
		}
		v.RequestCount += b.RequestCount
		v.ErrorCount += b.ErrorCount
		v.CallCount += b.CallCount
		v.InputTokens += b.InputTokens
		v.OutputTokens += b.OutputTokens
		v.TotalTokens += b.TotalTokens
		v.CacheReadTokens += b.CacheReadTokens
		v.CacheCreationTokens += b.CacheCreationTokens
		v.Cost += b.Cost
		v.RawCost += b.RawCost
		v.TTFTTotalMS += b.TTFTTotalMS
		v.TTFTCount += b.TTFTCount
		if b.TTFTMaxMS > v.TTFTMaxMS {
			v.TTFTMaxMS = b.TTFTMaxMS
		}
	}
	out := make([]*domain.EntityStatBucket, 0, len(m))
	for _, v := range m {
		c := *v
		out = append(out, &c)
	}
	slices.SortFunc(out, func(a, b *domain.EntityStatBucket) int { return a.BucketTime.Compare(b.BucketTime) })
	return out, nil
}

func (f *fakeStore) StatsTTFTSketch(ctx context.Context, from, to time.Time, model string) (*domain.TTFTSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total, count, maxMS int64
	for _, b := range f.stats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if model != "" && b.Model != model {
			continue
		}
		total += b.TTFTTotalMS
		count += b.TTFTCount
		if b.TTFTMaxMS > maxMS {
			maxMS = b.TTFTMaxMS
		}
	}
	avg := int64(0)
	if count > 0 {
		avg = (total + count/2) / count
	}
	return &domain.TTFTSummary{Count: count, AvgMS: avg, MaxMS: maxMS, Source: "sketch"}, nil
}

func (f *fakeStore) StatsTTFTExact(ctx context.Context, from, to time.Time, entityType string, entityID int64, model string) (*domain.TTFTSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var total, count, maxMS int64
	for _, b := range f.entityStats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if b.EntityType != entityType || b.EntityID != entityID {
			continue
		}
		if model != "" && b.Model != model {
			continue
		}
		total += b.TTFTTotalMS
		count += b.TTFTCount
		if b.TTFTMaxMS > maxMS {
			maxMS = b.TTFTMaxMS
		}
	}
	avg := int64(0)
	if count > 0 {
		avg = (total + count/2) / count
	}
	return &domain.TTFTSummary{Count: count, AvgMS: avg, MaxMS: maxMS, Source: "exact"}, nil
}

// --- /api/admin/overview 聚合面（与真实 StatRepo 同语义：区间 + 组过滤；毫分原样） ---

func (f *fakeStore) SummarizeStats(ctx context.Context, from, to time.Time, groupID int64) (*repository.StatSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &repository.StatSummary{}
	for _, b := range f.stats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if groupID > 0 && b.GroupID != groupID {
			continue
		}
		s.Requests += b.RequestCount
		s.Errors += b.ErrorCount
		s.InputTokens += b.InputTokens
		s.OutputTokens += b.OutputTokens
		s.TotalTokens += b.TotalTokens
		s.CacheReadTokens += b.CacheReadTokens
		s.Cost += b.Cost
	}
	return s, nil
}

func (f *fakeStore) ScanStatsDays(ctx context.Context, from, to time.Time, groupID int64) ([]*repository.StatDayAgg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	day := map[string]*repository.StatDayAgg{}
	var order []string
	for _, b := range f.stats {
		if b.BucketTime.Before(from) || !b.BucketTime.Before(to) {
			continue
		}
		if groupID > 0 && b.GroupID != groupID {
			continue
		}
		k := b.BucketTime.UTC().Truncate(24 * time.Hour).Format("2006-01-02")
		a, ok := day[k]
		if !ok {
			a = &repository.StatDayAgg{Date: b.BucketTime.UTC().Truncate(24 * time.Hour)}
			day[k] = a
			order = append(order, k)
		}
		a.Requests += b.RequestCount
		a.Errors += b.ErrorCount
		a.Tokens += b.TotalTokens
		a.Cost += b.Cost
	}
	out := make([]*repository.StatDayAgg, 0, len(day))
	for _, k := range order {
		out = append(out, day[k])
	}
	return out, nil
}

func (f *fakeStore) CountOverviewResources(ctx context.Context) (*repository.OverviewResourceCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &repository.OverviewResourceCounts{
		Templates: len(f.tpls),
		Groups:    len(f.groups),
		Users:     len(f.users),
	}, nil
}

func (f *fakeStore) ListUserEmails(ctx context.Context, ids []int64) (map[int64]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		if u, ok := f.users[id]; ok {
			out[id] = u.Email
		}
	}
	return out, nil
}

// --- 规则（RuleStore）：priority/name 唯一冲突模拟真实 repo 的 ErrConflict ---

func (f *fakeStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b domain.Rule) int { return a.Priority - b.Priority })
	return out, nil
}

func (f *fakeStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ruleConflictLocked(0, r); err != nil {
		return 0, err
	}
	r.ID = f.nextID
	f.nextID++
	f.rules[r.ID] = r
	return r.ID, nil
}

func (f *fakeStore) UpdateRule(ctx context.Context, r domain.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[r.ID]; !ok {
		return missingErr(r.ID)
	}
	if err := f.ruleConflictLocked(r.ID, r); err != nil {
		return err
	}
	f.rules[r.ID] = r
	return nil
}

func (f *fakeStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return missingErr(id)
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.rules[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.rules, id)
	}
	return nil
}

func (f *fakeStore) CountRules(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rules)), nil
}

// ruleConflictLocked 检查 priority/name 唯一冲突（持锁调用；excludeID 为更新目标自身）。
func (f *fakeStore) ruleConflictLocked(excludeID int64, r domain.Rule) error {
	for _, e := range f.rules {
		if e.ID != excludeID && (e.Priority == r.Priority || e.Name == r.Name) {
			return fmt.Errorf("%w: priority=%d or name=%q", repository.ErrConflict, r.Priority, r.Name)
		}
	}
	return nil
}

// templateNameConflictLocked 检查模板 name 唯一冲突（持锁调用；excludeID 为
// 更新目标自身；与真实 repo 的 ErrConflict 同格式）。
func (f *fakeStore) templateNameConflictLocked(excludeID int64, name string) error {
	for _, e := range f.tpls {
		if e.ID != excludeID && e.Name == name {
			return fmt.Errorf("%w: name=%q", repository.ErrConflict, name)
		}
	}
	return nil
}

// groupNameConflictLocked 检查分组 name 唯一冲突（持锁调用；excludeID 为更新
// 目标自身；与真实 repo 的 ErrConflict 同格式）。
func (f *fakeStore) groupNameConflictLocked(excludeID int64, name string) error {
	for _, e := range f.groups {
		if e.ID != excludeID && e.Name == name {
			return fmt.Errorf("%w: name=%q", repository.ErrConflict, name)
		}
	}
	return nil
}

// --- Phase 3a：UserStore / SettingStore 假实现 ---

func (f *fakeStore) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u.ID = f.nextID
	f.nextID++
	c := *u
	f.users[u.ID] = &c
	return &c, nil
}

func (f *fakeStore) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *u
	return &c, nil
}

func (f *fakeStore) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Email == email {
			c := *u
			return &c, nil
		}
	}
	return nil, nil
}

// CountUsers 用户总数（注册 bootstrap：表空 = 首个注册 = platform_admin）。
func (f *fakeStore) CountUsers(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.users)), nil
}

func (f *fakeStore) ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.User
	for _, u := range f.users {
		if q.Email != "" && !strings.Contains(u.Email, q.Email) {
			continue
		}
		c := *u
		out = append(out, &c)
	}
	return paginate(out, q), int64(len(out)), nil
}

func (f *fakeStore) UpdateUser(ctx context.Context, p *repository.UserPatch) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.users[p.ID]
	if !ok {
		return nil, missingErr(p.ID)
	}
	// 条件更新语义（对齐真实 repo）：balance/max_concurrency 显式设置时旧值
	// 不满足（期间有扣费/并发变更）→ ErrConflict。
	if p.MaxConcurrency != nil {
		if p.OldMaxConcurrency == nil || cur.MaxConcurrency != *p.OldMaxConcurrency {
			return nil, fmt.Errorf("%w: id=%d max_concurrency changed", repository.ErrConflict, p.ID)
		}
		cur.MaxConcurrency = *p.MaxConcurrency
	}
	if p.Balance != nil {
		if p.OldBalance == nil || cur.Balance != *p.OldBalance {
			return nil, fmt.Errorf("%w: id=%d balance changed", repository.ErrConflict, p.ID)
		}
		cur.Balance = *p.Balance
	}
	if p.Role != nil {
		cur.Role = *p.Role
	}
	if p.Status != nil {
		cur.Status = *p.Status
	}
	c := *cur
	return &c, nil
}

func (f *fakeStore) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return missingErr(id)
	}
	u.PasswordHash = passwordHash
	return nil
}

// CreateTempBalance 临时额度行（注册赠品；handler 测试无断言需求，静默成功）。
func (f *fakeStore) CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[userID]; !ok {
		return missingErr(userID)
	}
	return nil
}

// ListUserTempBalances 用户侧有效临时额度（编译兜底：fake 不持久化 temps，
// 恒空——临时额度行为断言走真实 PG 测试）。
func (f *fakeStore) ListUserTempBalances(ctx context.Context, userID int64) ([]*domain.TempBalance, error) {
	return nil, nil
}

// ListTempBalances 管理侧全量临时额度（编译兜底：同 ListUserTempBalances）。
func (f *fakeStore) ListTempBalances(ctx context.Context, q repository.ListQuery, userID int64) ([]*domain.TempBalance, int64, error) {
	return nil, 0, nil
}

func (f *fakeStore) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.settings[key]; ok {
		c := *s
		return &c, nil
	}
	return domain.DefaultSetting(key), nil
}

func (f *fakeStore) GetAllSettings(ctx context.Context) ([]*domain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.Setting
	for _, d := range domain.DefaultSettings {
		if s, ok := f.settings[d.Key]; ok {
			c := *s
			out = append(out, &c)
		} else {
			dd := d
			out = append(out, &dd)
		}
	}
	return out, nil
}

func (f *fakeStore) SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[key] = &domain.Setting{Key: key, Type: typ, Value: value}
	return &domain.Setting{Key: key, Type: typ, Value: value}, nil
}

// --- Phase 3a Task 4：KeyStore / GroupAssignmentStore 假实现 ---

func (f *fakeStore) CreateKey(ctx context.Context, k *domain.Key) (*domain.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k.ID = f.nextID
	f.nextID++
	c := *k
	f.keys[k.ID] = &c
	out := c // 返回独立副本：存库条目一经写入不再被外部指针修改
	return &out, nil
}

func (f *fakeStore) GetKey(ctx context.Context, id int64) (*domain.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *k
	return &c, nil
}

func (f *fakeStore) ListKeysByUser(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.Key, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.Key
	for _, k := range f.keys {
		if k.DeletedAt != nil || k.UserID != userID {
			continue
		}
		if q.Name != "" && !strings.Contains(k.Name, q.Name) {
			continue
		}
		c := *k
		out = append(out, &c)
	}
	return paginate(out, q), int64(len(out)), nil
}

// paginate 测试 fake 的分页裁剪（镜像真实 repo 语义：Limit ≤0 → 20、
// Offset <0 → 0；total 由调用方用裁剪前全量返回）。
func paginate[T any](rows []T, q repository.ListQuery) []T {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	total := len(rows)
	if q.Offset >= total {
		return nil
	}
	end := q.Offset + q.Limit
	if end > total {
		end = total
	}
	return rows[q.Offset:end]
}

// ListKeys 管理端全量 key 列表（/api/admin/keys：name 模糊 + user_id/group_id
// 等值 AND 组合 + limit/offset 裁剪；软删过滤——fake 的 DeleteKey 即置
// deleted_at，行保留。total 恒为满足筛选全量，不分页裁剪）。
func (f *fakeStore) ListKeys(ctx context.Context, q repository.ListQuery) ([]*domain.Key, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.Key
	for _, k := range f.keys {
		if k.DeletedAt != nil {
			continue
		}
		if q.Name != "" && !strings.Contains(k.Name, q.Name) {
			continue
		}
		if q.UserID > 0 && k.UserID != q.UserID {
			continue
		}
		if q.GroupID > 0 && k.GroupID != q.GroupID {
			continue
		}
		c := *k
		out = append(out, &c)
	}
	total := len(out)
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.Offset >= total {
		out = nil
	} else if end := q.Offset + q.Limit; end < total {
		out = out[q.Offset:end]
	} else {
		out = out[q.Offset:]
	}
	return out, int64(total), nil
}

// UpdateKey patch 语义（S3-F1，镜像真实 repo）：仅应用非 nil 字段，nil = 不动。
func (f *fakeStore) UpdateKey(ctx context.Context, p *repository.KeyPatch) (*domain.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.keys[p.ID]
	if !ok {
		return nil, missingErr(p.ID)
	}
	if p.Name != nil {
		cur.Name = *p.Name
	}
	if p.Status != nil {
		cur.Status = *p.Status
	}
	if p.MaxConcurrency != nil {
		cur.MaxConcurrency = *p.MaxConcurrency
	}
	if p.Quota != nil {
		cur.Quota = *p.Quota
	}
	c := *cur
	return &c, nil
}

func (f *fakeStore) RotateKey(ctx context.Context, id int64, newRaw string) (*domain.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.keys[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *cur
	c.KeyRaw = newRaw
	f.keys[id] = &c // 替换存库条目：旧引用不受影响
	out := c
	return &out, nil
}

// DeleteKey 软删语义（镜像真实 repo：行保留 + deleted_at 置值；GET 单个仍可
// 查已删项——ownedKey 的软删过滤因此真实可测）。
func (f *fakeStore) DeleteKey(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k, ok := f.keys[id]
	if !ok {
		return missingErr(id)
	}
	now := time.Now()
	k.DeletedAt = &now
	return nil
}

func (f *fakeStore) GrantGroup(ctx context.Context, groupID, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.assign[groupID], userID) {
		f.assign[groupID] = append(f.assign[groupID], userID)
	}
	return nil
}

func (f *fakeStore) RevokeGroup(ctx context.Context, groupID, userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assign[groupID] = slices.DeleteFunc(f.assign[groupID], func(u int64) bool { return u == userID })
	delete(f.assignMult, [2]int64{groupID, userID})
	return nil
}

// SetAssignmentMultiplier 设置/清除该用户在该组的专属价格倍率（T3.5 修正：
// 按组；nil = 清除为未设置 → 回退组倍率）。
func (f *fakeStore) SetAssignmentMultiplier(ctx context.Context, groupID, userID int64, m *int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.assign[groupID], userID) {
		return missingErr(userID)
	}
	f.assignMult[[2]int64{groupID, userID}] = m
	return nil
}

func (f *fakeStore) ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.GroupAssignment
	for gid, users := range f.assign {
		for _, u := range users {
			if u == userID {
				out = append(out, &domain.GroupAssignment{
					GroupID: gid, UserID: userID,
					PriceMultiplier: f.assignMult[[2]int64{gid, userID}],
				})
			}
		}
	}
	return out, nil
}

func (f *fakeStore) ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.GroupAssignment
	for _, u := range f.assign[groupID] {
		out = append(out, &domain.GroupAssignment{
			GroupID: groupID, UserID: u,
			PriceMultiplier: f.assignMult[[2]int64{groupID, u}],
		})
	}
	return out, nil
}

func (f *fakeStore) ListGroupsForUser(ctx context.Context, userID int64) ([]*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.Group
	for _, g := range f.groups {
		if g.DeletedAt != nil {
			continue // 软删组不进可选列表（真实 repo 同谓词）
		}
		if g.Visibility == domain.GroupVisibilityPublic {
			c := *g
			out = append(out, &c)
			continue
		}
		for _, u := range f.assign[g.ID] {
			if u == userID {
				c := *g
				out = append(out, &c)
				break
			}
		}
	}
	return out, nil
}

// --- 兑换码（RedemptionStore，Task 2 service.Store 扩展；语义与 service 包 fake 对齐） ---

// WithTx 事务语义模拟（评审 I-1）：fn 内变更先入暂存（fakeTx 持有深拷贝），
// fn 返回 nil → 提交（整体替换主视图），返回错误 → 丢弃。持锁贯穿整个事务。
func (f *fakeStore) WithTx(ctx context.Context, fn func(repository.TxStore) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := &fakeTx{
		codes:  cloneCodeMap(f.codes),
		uses:   cloneUseMap(f.uses),
		users:  cloneUserMap(f.users),
		temps:  slices.Clone(f.temps),
		nextID: f.nextID,
		// 授予面（S3-F2：assignment 替换循环入事务）
		assign:     cloneAssignMap(f.assign),
		assignMult: maps.Clone(f.assignMult),
		// 账号/扩展/归组面（Task B codex 导入 imported 行单行事务）
		accs:      cloneAccMap(f.accs),
		accExts:   cloneAccExtMap(f.accExts),
		accGroups: cloneAccGroupsMap(f.accGroups),
		groups:    cloneGroupMap(f.groups),
	}
	if err := fn(tx); err != nil {
		return err // 回滚：暂存丢弃
	}
	f.codes, f.uses, f.users, f.temps, f.nextID = tx.codes, tx.uses, tx.users, tx.temps, tx.nextID
	f.assign, f.assignMult = tx.assign, tx.assignMult
	f.accs, f.accExts, f.accGroups = tx.accs, tx.accExts, tx.accGroups
	return nil
}

func cloneAssignMap(m map[int64][]int64) map[int64][]int64 {
	out := make(map[int64][]int64, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

// fakeTempRow 临时额度行模拟（CreateTempBalance 指针参数即全字段）。
type fakeTempRow struct {
	UserID    int64
	Amount    int64
	ExpiresAt *time.Time
	Note      *string
}

type fakeTx struct {
	codes  map[int64]*domain.RedemptionCode
	uses   map[int64]*domain.RedemptionUse
	users  map[int64]*domain.User
	temps  []*fakeTempRow
	nextID int64
	// 授予面（S3-F2）：assign/assignMult 同 fakeStore 语义。
	assign     map[int64][]int64
	assignMult map[[2]int64]*int
	// 账号/扩展/归组面（Task B codex 导入 imported 行单行事务）：同 fakeStore
	// 语义。
	accs      map[int64]*domain.Account
	accExts   map[int64]*domain.AccountExt
	accGroups map[int64][]int64
	groups    map[int64]*domain.Group
}

var _ repository.TxStore = (*fakeTx)(nil)

func cloneCodeMap(m map[int64]*domain.RedemptionCode) map[int64]*domain.RedemptionCode {
	out := make(map[int64]*domain.RedemptionCode, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneUseMap(m map[int64]*domain.RedemptionUse) map[int64]*domain.RedemptionUse {
	out := make(map[int64]*domain.RedemptionUse, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneUserMap(m map[int64]*domain.User) map[int64]*domain.User {
	out := make(map[int64]*domain.User, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneAccMap(m map[int64]*domain.Account) map[int64]*domain.Account {
	out := make(map[int64]*domain.Account, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneAccExtMap(m map[int64]*domain.AccountExt) map[int64]*domain.AccountExt {
	out := make(map[int64]*domain.AccountExt, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

func cloneAccGroupsMap(m map[int64][]int64) map[int64][]int64 {
	out := make(map[int64][]int64, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

func cloneGroupMap(m map[int64]*domain.Group) map[int64]*domain.Group {
	out := make(map[int64]*domain.Group, len(m))
	for k, v := range m {
		c := *v
		out[k] = &c
	}
	return out
}

// --- 账号/扩展/归组（Task B codex 导入 imported 行单行事务面；语义镜像
// fakeStore 对应方法——变更只落暂存） ---

func (t *fakeTx) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	a.ID = t.nextID
	t.nextID++
	c := *a
	t.accs[a.ID] = &c
	return a, nil
}

// SetAccountGroups 替换账号的全部分组（镜像真实 repo：组存在性先校验——缺失
// → ErrNotFound 含 id；账号缺 id → ErrNotFound）。
func (t *fakeTx) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	for _, gid := range groupIDs {
		if _, ok := t.groups[gid]; !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, gid)
		}
	}
	if _, ok := t.accs[accountID]; !ok {
		return missingErr(accountID)
	}
	t.accGroups[accountID] = slices.Clone(groupIDs)
	return nil
}

func (t *fakeTx) UpsertAccountExt(ctx context.Context, e *domain.AccountExt) (*domain.AccountExt, error) {
	c := *e
	t.accExts[e.AccountID] = &c
	return &c, nil
}

func (t *fakeTx) FindAccountExtByCodexKey(ctx context.Context, codexEmail, codexAccountID string) (*domain.AccountExt, error) {
	for _, e := range t.accExts {
		if e.CodexEmail != nil && *e.CodexEmail == codexEmail &&
			e.CodexAccountID != nil && *e.CodexAccountID == codexAccountID {
			c := *e
			return &c, nil
		}
	}
	return nil, fmt.Errorf("%w: codex_email=%q codex_account_id=%q missing", repository.ErrNotFound, codexEmail, codexAccountID)
}

func (t *fakeTx) CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error {
	for _, c := range codes {
		for _, e := range t.codes {
			if e.Code == c.Code {
				return fmt.Errorf("%w: code 唯一冲突（批量插入全败）", repository.ErrConflict)
			}
		}
		c.ID = t.nextID // 回填 id（响应 {codes: [...]} 需完整可用）
		t.nextID++
		cc := *c
		t.codes[cc.ID] = &cc
	}
	return nil
}

func (t *fakeTx) GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error) {
	for _, c := range t.codes {
		if c.Code == code {
			cc := *c
			return &cc, nil
		}
	}
	return nil, fmt.Errorf("%w: code=%q", repository.ErrNotFound, code)
}

func (t *fakeTx) GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error) {
	for _, u := range t.uses {
		if u.CodeID == codeID && u.UserID == userID {
			cc := *u
			return &cc, nil
		}
	}
	return nil, fmt.Errorf("%w: code_id=%d user_id=%d", repository.ErrNotFound, codeID, userID)
}

func (t *fakeTx) UpdateUserBalance(ctx context.Context, userID, delta int64) error {
	u, ok := t.users[userID]
	if !ok {
		return missingErr(userID)
	}
	u.Balance += delta
	return nil
}

func (t *fakeTx) UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error {
	u, ok := t.users[userID]
	if !ok {
		return missingErr(userID)
	}
	if u.MaxConcurrency == 0 {
		u.MaxConcurrency = value
	} else {
		u.MaxConcurrency += value
	}
	return nil
}

func (t *fakeTx) CreateTempBalance(ctx context.Context, userID int64, amount int64, expiresAt *time.Time, note *string) error {
	t.temps = append(t.temps, &fakeTempRow{UserID: userID, Amount: amount, ExpiresAt: expiresAt, Note: note})
	return nil
}

func (t *fakeTx) CreateUse(ctx context.Context, use *domain.RedemptionUse) error {
	for _, u := range t.uses {
		if u.CodeID == use.CodeID && u.UserID == use.UserID {
			return fmt.Errorf("%w: code_id=%d user_id=%d", repository.ErrConflict, use.CodeID, use.UserID)
		}
	}
	c := *use
	c.ID = t.nextID
	t.nextID++
	t.uses[c.ID] = &c
	return nil
}

func (t *fakeTx) IncrementUsed(ctx context.Context, codeID int64) (bool, error) {
	c, ok := t.codes[codeID]
	if !ok {
		return false, nil
	}
	if c.UsedCount >= c.MaxUses {
		return false, nil
	}
	c.UsedCount++
	return true, nil
}

// --- 组授予（S3-F2：tx 面扩展，语义镜像 fakeStore 对应方法） ---

func (t *fakeTx) GrantGroup(ctx context.Context, groupID, userID int64) error {
	if !slices.Contains(t.assign[groupID], userID) {
		t.assign[groupID] = append(t.assign[groupID], userID)
	}
	return nil
}

func (t *fakeTx) RevokeGroup(ctx context.Context, groupID, userID int64) error {
	t.assign[groupID] = slices.DeleteFunc(t.assign[groupID], func(u int64) bool { return u == userID })
	delete(t.assignMult, [2]int64{groupID, userID})
	return nil
}

// SetAssignmentMultiplier 设置/清除该用户在该组的专属价格倍率（T3.5 修正：
// 按组；nil = 清除为未设置 → 回退组倍率）。
func (t *fakeTx) SetAssignmentMultiplier(ctx context.Context, groupID, userID int64, m *int) error {
	if !slices.Contains(t.assign[groupID], userID) {
		return missingErr(userID) // 授予行必须已存在（service 先 Grant 再 Set）
	}
	t.assignMult[[2]int64{groupID, userID}] = m
	return nil
}

func (t *fakeTx) ListAssignmentsByGroup(ctx context.Context, groupID int64) ([]*domain.GroupAssignment, error) {
	var out []*domain.GroupAssignment
	for _, u := range t.assign[groupID] {
		out = append(out, &domain.GroupAssignment{
			GroupID: groupID, UserID: u,
			PriceMultiplier: t.assignMult[[2]int64{groupID, u}],
		})
	}
	return out, nil
}

func (t *fakeTx) ListAssignmentsByUser(ctx context.Context, userID int64) ([]*domain.GroupAssignment, error) {
	var out []*domain.GroupAssignment
	for gid, users := range t.assign {
		for _, u := range users {
			if u == userID {
				out = append(out, &domain.GroupAssignment{
					GroupID: gid, UserID: userID,
					PriceMultiplier: t.assignMult[[2]int64{gid, userID}],
				})
			}
		}
	}
	return out, nil
}

func (f *fakeStore) CreateCodes(ctx context.Context, codes []*domain.RedemptionCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range codes {
		for _, e := range f.codes {
			if e.Code == c.Code {
				return fmt.Errorf("%w: code 唯一冲突（批量插入全败）", repository.ErrConflict)
			}
		}
		c.ID = f.nextID // 回填 id（响应 {codes: [...]} 需完整可用）
		f.nextID++
		cc := *c
		f.codes[cc.ID] = &cc
	}
	return nil
}

func (f *fakeStore) GetByCode(ctx context.Context, code string) (*domain.RedemptionCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.codes {
		if c.Code == code {
			cc := *c
			return &cc, nil
		}
	}
	return nil, fmt.Errorf("%w: code=%q", repository.ErrNotFound, code)
}

func (f *fakeStore) GetCode(ctx context.Context, id int64) (*domain.RedemptionCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[id]
	if !ok {
		return nil, missingErr(id)
	}
	cc := *c
	return &cc, nil
}

func (f *fakeStore) ListCodes(ctx context.Context, q repository.ListQuery, typ *domain.RedemptionType, status *domain.RedemptionStatus) ([]*domain.RedemptionCode, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.RedemptionCode
	for _, c := range f.codes {
		if typ != nil && c.Type != *typ {
			continue
		}
		if status != nil && c.Status != *status {
			continue
		}
		cc := *c
		out = append(out, &cc)
	}
	return out, int64(len(out)), nil
}

func (f *fakeStore) ListCodeUses(ctx context.Context, codeID int64, q repository.ListQuery) ([]*domain.RedemptionUse, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.RedemptionUse
	for _, u := range f.uses {
		if u.CodeID != codeID {
			continue
		}
		c := *u
		out = append(out, &c)
	}
	// 镜像真实 repo 缺省排序（sort=id, order=desc）——offset 翻页需确定性顺序。
	slices.SortFunc(out, func(a, b *domain.RedemptionUse) int { return cmp.Compare(b.ID, a.ID) })
	total := len(out)
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.Offset >= total {
		out = nil
	} else if end := q.Offset + q.Limit; end < total {
		out = out[q.Offset:end]
	} else {
		out = out[q.Offset:]
	}
	return out, int64(total), nil
}

// ListUsesByUser 某用户的兑换记录（/api/user/redemptions）：use + 码联查（码的
// type/remark 随记录返回，对齐真实 repo 的 WithCode 边）。
func (f *fakeStore) ListUsesByUser(ctx context.Context, userID int64, q repository.ListQuery) ([]*domain.RedemptionRecord, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.RedemptionRecord
	for _, u := range f.uses {
		if u.UserID != userID {
			continue
		}
		rec := &domain.RedemptionRecord{
			ID: u.ID, CodeID: u.CodeID, Value: u.Value,
			ResourceExpiresAt: u.ResourceExpiresAt, CreatedAt: u.CreatedAt,
		}
		if code, ok := f.codes[u.CodeID]; ok {
			rec.Code = code.Code
			rec.CodeType = code.Type
			rec.Remark = code.Remark
		}
		out = append(out, rec)
	}
	return out, int64(len(out)), nil
}

func (f *fakeStore) GetUse(ctx context.Context, codeID, userID int64) (*domain.RedemptionUse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.uses {
		if u.CodeID == codeID && u.UserID == userID {
			c := *u
			return &c, nil
		}
	}
	return nil, fmt.Errorf("%w: code_id=%d user_id=%d", repository.ErrNotFound, codeID, userID)
}

func (f *fakeStore) CreateUse(ctx context.Context, use *domain.RedemptionUse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.uses {
		if u.CodeID == use.CodeID && u.UserID == use.UserID {
			return fmt.Errorf("%w: code_id=%d user_id=%d", repository.ErrConflict, use.CodeID, use.UserID)
		}
	}
	c := *use
	c.ID = f.nextID
	f.nextID++
	f.uses[c.ID] = &c
	return nil
}

func (f *fakeStore) IncrementUsed(ctx context.Context, codeID int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.codes[codeID]
	if !ok {
		return false, nil
	}
	if c.UsedCount >= c.MaxUses {
		return false, nil
	}
	c.UsedCount++
	return true, nil
}

func (f *fakeStore) DeactivateCodes(ctx context.Context, ids []int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(ids) == 0 {
		return 0, nil
	}
	var n int64
	for _, id := range ids {
		c, ok := f.codes[id]
		if !ok {
			continue
		}
		if c.Status == domain.RedemptionStatusDisabled {
			continue
		}
		c.Status = domain.RedemptionStatusDisabled
		n++
	}
	return n, nil
}

// --- 原子资源更新（UserStore 扩展，评审 I-1） ---

func (f *fakeStore) UpdateUserBalance(ctx context.Context, userID, delta int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return missingErr(userID)
	}
	u.Balance += delta
	return nil
}

func (f *fakeStore) UpdateUserMaxConcurrency(ctx context.Context, userID int64, value int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return missingErr(userID)
	}
	if u.MaxConcurrency == 0 {
		u.MaxConcurrency = value
	} else {
		u.MaxConcurrency += value
	}
	return nil
}

// --- 模型价格（service.PricingStore，Phase 5 计费价格来源） ---

// UpsertFromLiteLLM 模拟批量 upsert + manual 行级互斥（已存在 manual 行不覆盖、
// 不计入更新数；source 恒 litellm）。

// UpsertManual 模拟手动设价（可接管 litellm 行；返回副本）。可选字段
// （cache 价 + 矩阵 22 列）：nil = 不设价（NULL 语义）；非 nil = 设价。

// DeleteManual 模拟删除：仅 source=manual 可删（litellm 行 → ErrConflict）；
// 不存在 → ErrNotFound。

// ListPricing 模拟列表：source/provider/model 筛选 + model 排序 + 分页。

// GetPricing 模拟按 model 取行（缺失 → repository.ErrNotFound）。

// --- 图片生成价格（service.PricingStore Task A 双线；与文本价同款模拟语义） ---

// UpsertImageFromLiteLLM 模拟批量 upsert + manual 行级互斥（已存在 manual 行
// 不覆盖、不计入更新数；source 恒 litellm）。

// UpsertImageManual 模拟手动设图价格（可接管 litellm 行；返回副本）。

// DeleteImageManual 模拟删除：仅 source=manual 可删（litellm 行 → ErrConflict）；
// 不存在 → ErrNotFound。

// ListImagePrice 模拟列表：source/provider/model 筛选 + model 排序 + 分页。

// GetImagePrice 模拟按 model 取行（缺失 → repository.ErrNotFound）。

// --- 按单元计费功能类价格（价格表三件套；与文本价/image 价同款模拟语义） ---

// UpsertFunctionFromLiteLLM 模拟批量 upsert + manual 行级互斥（已存在 manual
// 行不覆盖、不计入更新数；source 恒 litellm）。

// UpsertFunctionManual 模拟手动设按单元价（可接管 litellm 行；返回副本）。

// DeleteFunctionManual 模拟删除：仅 source=manual 可删（litellm 行 → ErrConflict）；
// 不存在 → ErrNotFound。

// ListFunctionPrice 模拟列表：source/provider/model 筛选 + model 排序 + 分页。

// GetFunctionPrice 模拟按 model 取行（缺失 → repository.ErrNotFound）。

func (f *fakeStore) UpsertPriceEntriesFromLiteLLM(_ context.Context, rows []*domain.PriceEntry) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range rows {
		if cur, ok := f.priceEntries[r.Model]; ok && cur.Source == domain.PricingSourceManual {
			continue
		}
		f.priceEntries[r.Model] = r
		n++
	}
	return n, nil
}
func (f *fakeStore) UpsertPriceVariantsFromLiteLLM(_ context.Context, variants []*domain.PriceVariant) (int, error) {
	byModel := map[string][]*domain.PriceVariant{}
	for _, v := range variants {
		byModel[v.Model] = append(byModel[v.Model], v)
	}
	total := 0
	for m, lst := range byModel {
		f.priceVariants[m] = lst
		total += len(lst)
	}
	return total, nil
}
func (f *fakeStore) UpsertPriceEntryManual(_ context.Context, m *repository.PriceEntryManual) (*domain.PriceEntry, error) {
	pe := &domain.PriceEntry{Model: m.Model, Mode: m.Mode, InputPerM: m.InputPerM, OutputPerM: m.OutputPerM, CacheReadPerM: m.CacheReadPerM, CacheWritePerM: m.CacheWritePerM, PricePerCall: m.PricePerCall, ImgInTokPerM: m.ImgInTokPerM, ImgOutTokPerM: m.ImgOutTokPerM, PricePerImage: m.PricePerImage, Source: domain.PricingSourceManual}
	f.priceEntries[m.Model] = pe
	return pe, nil
}
func (f *fakeStore) DeletePriceEntryManual(_ context.Context, model string) error {
	if _, ok := f.priceEntries[model]; !ok {
		return fmt.Errorf("%w: model=%q", repository.ErrNotFound, model)
	}
	if f.priceEntries[model].Source != domain.PricingSourceManual {
		return fmt.Errorf("%w: model=%q source=litellm", repository.ErrConflict, model)
	}
	delete(f.priceEntries, model)
	delete(f.priceVariants, model)
	return nil
}
func (f *fakeStore) ListPriceEntries(_ context.Context, q repository.ListQuery, source *domain.PricingSource, mode *domain.PriceMode, model string) ([]*domain.PriceEntry, int64, error) {
	var all []*domain.PriceEntry
	for _, e := range f.priceEntries {
		if source != nil && e.Source != *source {
			continue
		}
		if mode != nil && e.Mode != *mode {
			continue
		}
		if model != "" && !strings.Contains(strings.ToLower(e.Model), strings.ToLower(model)) {
			continue
		}
		all = append(all, e)
	}
	return all, int64(len(all)), nil
}
func (f *fakeStore) GetPriceEntry(_ context.Context, model string) (*domain.PriceEntry, error) {
	if e, ok := f.priceEntries[model]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("%w: model=%q", repository.ErrNotFound, model)
}
func (f *fakeStore) ListPriceVariants(_ context.Context, model string) ([]*domain.PriceVariant, error) {
	return f.priceVariants[model], nil
}
func (f *fakeStore) ListAllPriceVariants(_ context.Context) ([]*domain.PriceVariant, error) {
	var all []*domain.PriceVariant
	for _, lst := range f.priceVariants {
		all = append(all, lst...)
	}
	return all, nil
}
func (f *fakeStore) ReplacePriceVariants(_ context.Context, model string, variants []*domain.PriceVariant) ([]*domain.PriceVariant, error) {
	for _, v := range variants {
		if v.MultBP == nil && v.SetInputPerM == nil && v.SetOutputPerM == nil {
			return nil, fmt.Errorf("variant seq %d requires at least one effect", v.Seq)
		}
	}
	f.priceVariants[model] = variants
	return variants, nil
}

func emailCodeKeyHandler(email, purpose string) string { return email + "|" + purpose }

func (f *fakeStore) GetEmailTemplate(ctx context.Context, purpose string) (*domain.EmailTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.emailTemplates[purpose]; ok {
		c := *t
		return &c, nil
	}
	return nil, nil
}
func (f *fakeStore) ListEmailTemplates(ctx context.Context) ([]*domain.EmailTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.EmailTemplate
	for _, t := range f.emailTemplates {
		c := *t
		out = append(out, &c)
	}
	return out, nil
}
func (f *fakeStore) UpsertEmailTemplate(ctx context.Context, purpose, subject, bodyText string) (*domain.EmailTemplate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &domain.EmailTemplate{Purpose: domain.EmailTemplatePurpose(purpose), Subject: subject, BodyText: bodyText, UpdatedAt: time.Now()}
	f.emailTemplates[purpose] = t
	c := *t
	return &c, nil
}
func (f *fakeStore) DeleteEmailTemplate(ctx context.Context, purpose string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.emailTemplates[purpose]; !ok {
		return fmt.Errorf("%w: purpose=%s", repository.ErrNotFound, purpose)
	}
	delete(f.emailTemplates, purpose)
	return nil
}
func (f *fakeStore) GetEmailCode(ctx context.Context, email, purpose string) (*domain.EmailCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.emailCodes[emailCodeKeyHandler(email, purpose)]; ok {
		cc := *c
		return &cc, nil
	}
	return nil, nil
}
func (f *fakeStore) UpsertEmailCode(ctx context.Context, email, purpose, sha256 string, expiresAt time.Time) (*domain.EmailCode, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := emailCodeKeyHandler(email, purpose)
	now := time.Now()
	if existing, ok := f.emailCodes[key]; ok {
		existing.CodeSHA256 = sha256
		existing.ExpiresAt = expiresAt
		existing.Attempts = 0
		existing.UpdatedAt = now
		cc := *existing
		return &cc, nil
	}
	c := &domain.EmailCode{ID: f.nextID, Email: email, Purpose: domain.EmailCodePurpose(purpose), CodeSHA256: sha256, ExpiresAt: expiresAt, Attempts: 0, CreatedAt: now, UpdatedAt: now}
	f.nextID++
	f.emailCodes[key] = c
	cc := *c
	return &cc, nil
}
func (f *fakeStore) IncrementEmailCodeAttempts(ctx context.Context, email, purpose string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := emailCodeKeyHandler(email, purpose)
	c, ok := f.emailCodes[key]
	if !ok {
		return 0, fmt.Errorf("%w: email=%s purpose=%s", repository.ErrNotFound, email, purpose)
	}
	c.Attempts++
	c.UpdatedAt = time.Now()
	return c.Attempts, nil
}
func (f *fakeStore) DeleteEmailCode(ctx context.Context, email, purpose string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := emailCodeKeyHandler(email, purpose)
	if _, ok := f.emailCodes[key]; !ok {
		return fmt.Errorf("%w: email=%s purpose=%s", repository.ErrNotFound, email, purpose)
	}
	delete(f.emailCodes, key)
	return nil
}
