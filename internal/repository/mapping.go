// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
)

func toDomainUser(u *ent.User) *domain.User {
	return &domain.User{
		ID: u.ID, Email: u.Email, PasswordHash: u.PasswordHash,
		Role: domain.Role(u.Role), Status: domain.UserStatus(u.Status),
		TokenVersion:   u.TokenVersion,
		MaxConcurrency: u.MaxConcurrency, Balance: u.Balance,
		BalanceWarningThreshold: u.BalanceWarningThreshold,
		CreatedAt:               u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func toDomainKey(k *ent.Key) *domain.Key {
	d := &domain.Key{
		ID: k.ID, UserID: k.UserID, GroupID: k.GroupID, Name: k.Name,
		KeyRaw: k.KeyRaw,
		Status: domain.KeyStatus(k.Status), MaxConcurrency: k.MaxConcurrency,
		Quota: k.Quota, QuotaUsed: k.QuotaUsed,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt, DeletedAt: k.DeletedAt,
	}
	// 组名回填：仅当 eager-load 的组边非 nil（读路径经存活过滤挂边；写路径
	// 无组边 → nil → 留空）。软删组被过滤 → nil → 前端回退 `#<GroupID>`。
	if k.Edges.Group != nil {
		d.GroupName = k.Edges.Group.Name
	}
	return d
}

// toDomainManagementKey ent 行 → domain.ManagementKey（管理 key 面全量映射；
// 无 eager-load 边——owner 由 user_id 标量承载）。
func toDomainManagementKey(m *ent.ManagementKey) *domain.ManagementKey {
	return &domain.ManagementKey{
		ID: m.ID, UserID: m.UserID, Name: m.Name, KeyRaw: m.KeyRaw,
		Status:    domain.ManagementKeyStatus(m.Status),
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, DeletedAt: m.DeletedAt,
	}
}

func toDomainGroup(g *ent.Group) *domain.Group {
	return &domain.Group{
		ID: g.ID, Name: g.Name, Description: g.Description,
		Visibility:       domain.GroupVisibility(g.Visibility),
		PriceMultiplier:  g.PriceMultiplier,
		ProtocolConverts: toDomainProtocolConverts(g.ProtocolConvert),
		CreatedAt:        g.CreatedAt, UpdatedAt: g.UpdatedAt, DeletedAt: g.DeletedAt,
	}
}

// toDomainProtocolConverts ent JSON 数组（[]string）→ domain 协议转换方向集合
// （空数组/nil = off = 不转换）。
func toDomainProtocolConverts(v []string) []domain.ProtocolConvert {
	if len(v) == 0 {
		return nil
	}
	out := make([]domain.ProtocolConvert, len(v))
	for i, s := range v {
		out[i] = domain.ProtocolConvert(s)
	}
	return out
}

// protocolConvertStrings domain 方向集合 → ent JSON 数组（[]string；空集合 →
// 空数组落库——空数组 = off，与 nil/null 语义区分）。
func protocolConvertStrings(pcs []domain.ProtocolConvert) []string {
	if len(pcs) == 0 {
		return []string{}
	}
	out := make([]string, len(pcs))
	for i, pc := range pcs {
		out[i] = string(pc)
	}
	return out
}

func toDomainTemplateExt(e *ent.TemplateExt) *domain.TemplateExt {
	d := &domain.TemplateExt{
		TemplateID:              e.TemplateID,
		CredentialType:          credential.Type(e.CredentialType),
		StripImageTools:         e.StripImageTools,
		UpstreamPaths:           e.UpstreamPaths,
		CodexResponsesTransport: domain.CodexResponsesTransport(e.CodexResponsesTransport),
	}
	// request_patch 的读路径转换**只在这里**（与写路径的 CanonicalBytes 概念对称）：
	// domain.ParseRequestPatch 是 wire 校验的唯一入口，读面不再复制判据。
	//
	// 存量脏值（手工改库/旧形状）视为未配置：不报错、不 panic、不半应用
	// （spec §2.6「读路径遇到存量脏值…视为未配置」/§5.3）。脏值只可能来自绕过
	// 唯一写入口的 DB 直改——jsonb 列本身保证语法合法，wire 语义非法即在此忽略。
	if len(e.RequestPatch) > 0 {
		if p, err := domain.ParseRequestPatch(json.RawMessage(e.RequestPatch)); err == nil {
			d.RequestPatch = p
		}
	}
	return d
}

// toDomainAccountExt projects only the fields owned by the current provider.
func accountExtColumns(e *ent.AccountExt) *domain.AccountExt {
	if e == nil {
		return nil
	}
	return &domain.AccountExt{
		AccountID:              e.AccountID,
		CredentialType:         credential.Type(e.CredentialType),
		CodexOAuthToken:        e.CodexOauthToken,
		CodexOAuthRefreshToken: e.CodexOauthRefreshToken,
		CodexOAuthExpiresAt:    e.CodexOauthExpiresAt,
		CodexPATKey:            e.CodexPatKey,
		CodexEmail:             e.CodexEmail,
		CodexAccountID:         e.CodexAccountID,
	}
}

// ErrCredentialTypeMismatch 模板与 account_ext 行的 BPS 位不一致——一致性判据里
// **唯一有行为意义**的那一位（spec §0）。导出：PG 用例多在 repository_test 包，
// service 也用它把账号分流成"该账号不可用"。
var ErrCredentialTypeMismatch = errors.New("repository: account_ext credential type does not match the template")

// assertExtCredentialType 比较模板类型与 ext 行的凭据类型：**只比 BPS 位**
// （同通道内 codex-oauth ↔ codex-pat 在身份投影上行为等价，见 spec §0）。
//
// 它是全仓唯一允许在 ext 行类型上调用 IsBPS/IsCodex 的地方（门禁 G-B 的注册表
// 只有这一行）。account_id 由调用方用 %w 包装补上（本函数只拿得到两侧类型）。
func assertExtCredentialType(want, got credential.Type) error {
	if want.IsBPS() == got.IsBPS() {
		return nil
	}
	return fmt.Errorf("%w: template=%s ext=%s", ErrCredentialTypeMismatch, want, got)
}

// projectExtIdentity 是单行 ext 身份投影的唯一入口（快照合并、单读、额度面、
// 账号侧 ext 边共用）：模板为 Responses provider 时先断言 BPS 位一致，再按
// **模板类型**选分支；否则（无模板上下文 / 静态通道模板）分支退回 ext 行自身
// 类型（遗留行保持今天的行为）。
//
// 模板 BPS 位只判定这一次：分支选择与断言共用同一个 `IsResponsesProvider()`，
// 不抽 `accountExtBranchType` 之类的二次判定（S3b 谓词预算只许变短）。
func projectExtIdentity(ext *domain.AccountExt, env *domain.IdentityEnvelope, tplType *credential.Type) error {
	if tplType == nil || !tplType.IsResponsesProvider() {
		return projectAccountIdentity(ext, env, ext.CredentialType)
	}
	if err := assertExtCredentialType(*tplType, ext.CredentialType); err != nil {
		return fmt.Errorf("account_id=%d: %w", ext.AccountID, err)
	}
	return projectAccountIdentity(ext, env, *tplType)
}

// toDomainAccountExt ent ext 行 → domain 对象。branchType 只选身份投影分支，
// 不写回 ext.CredentialType。
func toDomainAccountExt(e *ent.AccountExt, branchType credential.Type) (*domain.AccountExt, error) {
	ext := accountExtColumns(e)
	if err := projectAccountIdentity(ext, e.CodexIdentity, branchType); err != nil {
		return nil, err
	}
	return ext, nil
}

// projectAccountIdentity 是身份投影的薄分发器：分支由调用方按**模板类型**
// （Responses provider）或遗留行的自身类型选好后传入，函数体**不读** ext 的类型
// （门禁 G-A：分支只许由模板决定）。
func projectAccountIdentity(ext *domain.AccountExt, env *domain.IdentityEnvelope, branchType credential.Type) error {
	if branchType.IsBPS() {
		return projectBPSIdentity(ext, env)
	}
	return projectCodexIdentity(ext, env)
}

// projectBPSIdentity BPS 身份投影：profile 来自 identity envelope，结构性校验
// 复用写面的 validateBPSExt（门禁 G-A 的活性锚点）。
func projectBPSIdentity(ext *domain.AccountExt, env *domain.IdentityEnvelope) error {
	if env != nil {
		ext.BPSProfile = (*env).BPSProfile()
	}
	if err := validateBPSExt(ext); err != nil {
		return fmt.Errorf("%w: account_id=%d: %s", errInvalidBPSAccountExt, ext.AccountID, err.Error())
	}
	return nil
}

// projectCodexIdentity codex 身份投影：只取 installation_id 一个字段。
func projectCodexIdentity(ext *domain.AccountExt, env *domain.IdentityEnvelope) error {
	if env == nil {
		return nil
	}
	id := &domain.CodexIdentity{}
	if raw, ok := (*env)["installation_id"]; ok {
		if err := json.Unmarshal(raw, &id.InstallationID); err != nil {
			return fmt.Errorf("account_id=%d: invalid installation_id in codex_identity", ext.AccountID)
		}
	}
	ext.CodexIdentity = id
	return nil
}

func toDomainGroupAssignment(a *ent.GroupAssignment) *domain.GroupAssignment {
	return &domain.GroupAssignment{
		ID: a.ID, GroupID: a.GroupID, UserID: a.UserID,
		PriceMultiplier: a.PriceMultiplier,
		CreatedAt:       a.CreatedAt,
	}
}

func toDomainSetting(s *ent.Setting) *domain.Setting {
	return &domain.Setting{
		ID: s.ID, Key: s.Key, Type: domain.SettingType(s.Type),
		Value: s.Value, UpdatedAt: s.UpdatedAt,
	}
}

func toDomainTemplate(t *ent.Template) *domain.Template {
	formats := make([]domain.RequestFormat, 0, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		formats = append(formats, domain.RequestFormat(f))
	}
	fm := make(map[domain.RequestFormat][]string, len(t.FormatModels))
	for k, v := range t.FormatModels {
		fm[domain.RequestFormat(k)] = v
	}
	d := &domain.Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL,
		CredentialType:   credential.Type(t.CredentialType),
		SupportedFormats: formats, Models: t.Models,
		FormatModels: fm, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, DeletedAt: t.DeletedAt,
	}
	// StripImageTools 快照合并：仅调度器快照加载（LoadGroupsAccounts /
	// LoadGroupAccounts 的 WithTemplate 嵌套 WithExt）会 eager-load ext 边；
	// 其余路径（管理面模板 CRUD 等）无 ext 边 → nil → false。管理面 ext 配置
	// 经 template_ext 端点单独读写，不合并进模板对象。
	if len(t.Edges.Ext) > 0 && t.Edges.Ext[0].StripImageTools != nil {
		d.StripImageTools = *t.Edges.Ext[0].StripImageTools
	}
	// 逐格式自定义路径同款快照合并（**仅 custom 模板**）：类型从 custom 改成别的
	// 类型时陈旧 ext 行不会被删除，若无条件合并，api_key 模板会继承旧路径并把
	// 请求打到非预期端点。按模板类型门控后，陈旧行对其它类型完全无效。
	if d.CredentialType == credential.TypeCustom && len(t.Edges.Ext) > 0 {
		d.UpstreamPaths = t.Edges.Ext[0].UpstreamPaths
	}
	// codex responses 出站传输同款快照合并（spec-codex-outbound-transport §4.4）。
	// **独立 if**：strip_image_tools 默认 NULL，把这段塞进上面的 strip 守卫里会让
	// 绝大多数模板的开关永远合并不进快照（现有夹具恰好 strip=true 时测试全绿，
	// 线上 ws 臂却静默回落 http）。
	// **门控只覆盖「当前类型不匹配」**：三个条件（有 ext 边、模板是 codex 类型、
	// ext 行类型与模板类型一致）都是对**此刻**配置的判定。它防不住「类型改走再
	// 改回与 ext 行相同的类型」——codex→custom→codex 之后陈旧 ext 行的类型重新
	// 与模板一致，旧的 ws 会就此复活；写面的 409 + 确认（§4.3）是那条路唯一的
	// 守卫，本函数不清理 ext 行（读路径不越权改写配置面）。
	// 值**原样**合并（不在合并点归一）：脏值由 Key()/Name() 按默认面归一；装载侧
	// 的审计 WARN 由 `scheduler.warnDirtyCodexTransports` 在每轮 reload 装配时写
	// （每模板一条，带 template_id + 原值）。
	if len(t.Edges.Ext) > 0 {
		ext := t.Edges.Ext[0]
		// 合并面 = 所有 Responses 原生 provider（codex 与 bps）。只认 codex 会让
		// BPS 模板的 ws 停在 ext 行里：写面能存、数据面永远走 HTTP。
		if (d.CredentialType.IsCodex() || d.CredentialType.IsBPS()) && ext.CredentialType == string(d.CredentialType) {
			d.CodexResponsesTransport = domain.CodexResponsesTransport(ext.CodexResponsesTransport)
		}
	}
	return d
}

// toDomainAccount ent 行 → domain 账号（模板**不共享**：每个账号各转一份）。
// 保留这个形状给所有"单行/列表"读面；快照载入走 toDomainAccountWithTemplateScope。
func toDomainAccount(a *ent.Account) *domain.Account {
	return toDomainAccountWithTemplateScope(a, nil)
}

// TemplateLoadScope 是一次**载入批次**的模板作用域：作用域内同一个 template_id
// 只从 ent 行转换一次，所有账号共享那一枚 *domain.Template。
//
// 生命周期由**调用方**按"一次构建 / 一个失效批次"持有（U22 §2.1）：
//
//   - 全量 reload：reloadLocked 建一张，贯穿该次构建的全部组；
//   - 组级失效：invalidateGroupsLocked 建一张，贯穿该批全部受影响组。这条是必须的
//     ——invalidate_batch 对每个受影响组各调一次单组载入（invalidate_batch.go:73-89），
//     逐次新建作用域会把"同模板跨组"退化成"每组一份指针"，按指针键的摘要缓存
//     随之退化成"每组每模板一次"；
//   - nil = 每次载入各建一张（既有调用点与测试不受影响）。
//
// **不是**包级、不是全局池：跨 reload 复用会把上一轮的模板对象带进新快照（模板
// 改动不生效），旧对象的存活期也会被无限拉长。
//
// 跨查询复用（同一批次里不同组的两次 Ent 查询各有一枚 ent 模板实体）同样按
// template_id 折叠，等价于把 Ent 的"同查询同 ID 同一实体"抬到批次级：批次内同一个
// 模板只有一份 domain 事实。代价是"批次中途模板行被改"时后一个组会沿用前一个组
// 读到的内容——这是一次构建/一个批次**该有**的行为（同一份快照不能半新半旧），
// 下一次 reload 整批换新。
//
// **非并发安全**：调用方保证同一作用域串行使用（reload / invalidate 都在调度器锁
// 内）。计数器只为测试与观测服务，不参与任何判据。
type TemplateLoadScope struct {
	templates map[int64]*domain.Template
	hits      int
	misses    int
}

// NewTemplateLoadScope 建一张空的载入作用域（调用方负责它的生命周期）。
func NewTemplateLoadScope() *TemplateLoadScope {
	return &TemplateLoadScope{templates: make(map[int64]*domain.Template)}
}

// Hits 返回作用域内复用已转换模板的次数（同模板的后续账号）。
func (s *TemplateLoadScope) Hits() int { return s.hits }

// Misses 返回作用域内实际转换 ent 模板的次数（= 出现过的不同 template_id 数）。
func (s *TemplateLoadScope) Misses() int { return s.misses }

// toDomainAccountWithTemplateScope 与 toDomainAccount 的唯一差别是模板转换：
// scope 非 nil 时按 `a.Edges.Template.ID` 复用作用域内已转好的 *domain.Template，
// 使同模板的账号共享同一枚指针（U22 §2.1：摘要缓存需要"一个模板一次 build 只有
// 一个对象"这个便宜的键）；nil scope = 不共享，与旧行为逐字节相同。
//
// 按模板 ID 复用是**安全**的，因为 Ent 的 eager-load 本身就按 ID 折叠：
// AccountQuery.loadTemplate 先按 TemplateID 去重外键，再把同一个模板实体分配给
// 该 ID 的所有账号（internal/ent/account_query.go:493-518）。所以同一次查询内
// 同 ID 的 `Edges.Template` 是同一枚实体，toDomainTemplate 末尾的 ext 合并
// （strip_image_tools / upstream_paths / codex transport）结果必然一致，
// 「复用第一份」不会丢差异。
//
// 共享的是**只读**对象。"模板不可变"不是我在这里新加的纪律，而是既有前提：
// toDomainTemplate 本来就把 ent 行的 Models / FormatModels / ModelMapping /
// UpstreamPaths 直接别名过去（不是深拷贝），所以"持有模板的人必须只读"在共享之前
// 就已成立。本单元只把"每账号一份"换成"每模板一份"，没有新增可变面；唯一的增量
// 风险是"写一处影响多个账号"，由构造期定型（scheduler.newSnapshotStatic）与
// AST 门禁（template_immutability_gate_test.go）共同兜住。
func toDomainAccountWithTemplateScope(a *ent.Account, scope *TemplateLoadScope) *domain.Account {
	tpl := toDomainAccountTemplate(a, scope)
	multBp := a.UpstreamCostMultiplierBp
	d := &domain.Account{
		ID: a.ID, Name: a.Name, TemplateID: a.TemplateID, Template: tpl,
		BaseURL:        a.BaseURL, // 账号级覆盖（nil = 继承模板；快照装配指针拷贝零分配）
		UpstreamKey:    a.UpstreamKey,
		MaxConcurrency: a.MaxConcurrency,
		LastError:      a.LastError, LastUsedAt: a.LastUsedAt,
		FailedAt:                 a.FailedAt,
		FailureSource:            a.FailureSource,
		Enabled:                  a.Enabled,
		LifecycleRevision:        a.LifecycleRevision,
		IdentityRevision:         a.IdentityRevision,
		UpstreamCostMultiplierBp: &multBp, // 读路径恒有值（列 NOT NULL + 默认）——取址拷贝，不别名 ent 行
		CacheDomain:              a.CacheDomain,
		CreatedAt:                a.CreatedAt, UpdatedAt: a.UpdatedAt, DeletedAt: a.DeletedAt,
	}
	// 供应商归属（spec §2.5/§4.6）：列 NULL = 平台自有（0）。非 nil 落值。
	if a.SupplierUserID != nil {
		d.SupplierUserID = *a.SupplierUserID
	}
	// 出口代理绑定（spec-proxy-pool §3.2）：列 NULL = 直连（nil）。本函数**只**投影
	// ProxyID 这一列，`d.Proxy` 恒留 nil：展开是读路径的事，且只有两处——
	// `account_repo.expandProxies`（账号读面：创建/单读/列表，scope-aware，「看不见的代理
	// 就不展开」）与 `group_repo.expandAccountProxy`（快照装载，全量）。本函数属于
	// "ent 行 → 领域对象"的最底层，把展开塞进来会让上面两处的口径混成一处。
	d.ProxyID = a.ProxyID
	// d.Ext 不在这里投影：账号侧 ext 边的唯一 eager-load 调用点是
	// account_repo.GetAccountWithTemplate（projectAccountExtEdge），快照路径则由
	// group_repo 的合并点写入（mergeExtSnapshotRow）——两处都需要**模板类型**才能
	// 选身份投影分支，本函数拿不到（spec J1/修正 1）。
	return d
}

// toDomainAccountTemplate 取账号的 domain 模板：无模板边 → nil；无作用域 → 现转
// 一份（不共享）；有作用域 → 作用域内同 ID 复用同一枚（首次出现时转换并登记）。
//
// 三个分支合在一处，是为了让"复用"只有一条路径：任何给模板转换加行为的人都必须
// 同时看到共享语义，而不是在两份转换代码里各写一遍。
func toDomainAccountTemplate(a *ent.Account, scope *TemplateLoadScope) *domain.Template {
	t := a.Edges.Template
	if t == nil {
		return nil
	}
	if scope == nil {
		return toDomainTemplate(t)
	}
	if tpl, ok := scope.templates[t.ID]; ok {
		scope.hits++
		return tpl
	}
	tpl := toDomainTemplate(t)
	scope.templates[t.ID] = tpl
	scope.misses++
	return tpl
}

// toDomainProxy ent 行 → domain.Proxy（可空列：nil 落零值——空串即无认证，
// 消费面无需区分 nil 与 ""；探测时刻保持指针，never-probed 必须可区分）。
func toDomainProxy(p *ent.Proxy) domain.Proxy {
	d := domain.Proxy{
		ID:             p.ID,
		Name:           p.Name,
		OwnerUID:       p.OwnerUID,
		Scheme:         domain.ProxyScheme(p.Scheme),
		Host:           p.Host,
		Port:           p.Port,
		Enabled:        p.Enabled,
		LastProbeAt:    p.LastProbeAt,
		LastProbeOK:    p.LastProbeOk,
		LastProbeError: p.LastProbeError,
		LastProbeIP:    p.LastProbeIP,
		// 管理面读模型回显（§6.1）：两列只读，不参与路由判定。
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	if p.Username != nil {
		d.Username = *p.Username
	}
	if p.Password != nil {
		d.Password = *p.Password
	}
	if p.Remark != nil {
		d.Remark = *p.Remark
	}
	return d
}
