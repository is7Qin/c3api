// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// planKey 是编译计划/决策的有效性判据（值语义）：已编译候选在预留与复用
// 检查中比较的是它，不是叶子指针。字段集 = 计划的全部静态输入——门禁与
// Selection 装配从当前叶读到的每个静态事实都落在这里；反之，落在 payloadKey
// 的载荷变化只换新叶子内容，不作废在途计划。
//
// 比较规则第 2 项（`CandidateFingerprint` 本身按值比较）在这里**未接线**：
// 它按值覆盖 accountID/templateID/credential_type/生效 baseURL/stripImageTools/
// upstreamKey/凭据摘要/凭据身份四元组/codexAccountID，这些事实**均已逐项**落在
// 下列字段里（digest 是 upstreamKey|patKey 的纯函数：domain/routing.go:231-257），
// 故覆盖面无缺口。
//
// 两枚 struct 都只含值语义字段（标量 / `[32]byte` 摘要），`==` 仍是唯一算子
// （沿用 static_key_test.go 的反射守卫）。
type planKey struct {
	// --- §5.5 账号侧（静态快照消费 = 是） ---
	accountID                int64
	name                     string
	templateID               int64
	baseURL                  string // 生效 baseURL：账号覆盖优先，否则模板值（覆盖率最广，同时覆盖 §5.5 的 base_url 行与 §5.5b 的 Template.BaseURL 行）
	upstreamKey              string
	maxConcurrency           int
	enabled                  bool
	cacheDomain              string // 规范化：nil 与 "" 对消费者等价（cacheDomainForAccount）
	upstreamCostMultiplierBp int
	identityRevision         int64 // K（spec §5.5 注：K 会进编译事实/候选/wire）

	// --- 凭据面消费子集（决策输入部分） ---
	//
	// codexEmail 落在这里：AccountCandidateFingerprint 把 Ext.CodexEmail 读入并
	// 传给 CandidateFingerprint——它位于决策路径，故 email 是决策输入。当前
	// hashFields 的实参清单里还没有它（死参数），所以今天 email 只是保守多失效
	// （email 仅管理面改写，SDK 自动刷新不碰它）；一旦有人把它真正接进指纹，
	// 判据无需改动即仍然正确。
	//
	// codexPATKey 落在这里：stableCredentialDigest 对 codex-pat 就是
	// sha256(patKey)，pat 轮转确实改变候选指纹 ⇒ 确实必须打断计划。而
	// codex-oauth 分支返回常量（domain/routing.go:249-253），故 OAuth 令牌不是
	// 决策输入——两个分支的差异正是"载荷 vs 决策输入"的分界线，键的拆分与
	// 指纹函数的拆分同源。
	codexAccountID    string
	codexInstallation string
	codexSession      string
	codexThread       string
	codexWindow       string
	codexEmail        string
	codexPATKey       string // 凭据值：pat（api_key/pat 类型上游鉴权用；同时进候选指纹）

	// --- §5.5b(1) 模板侧源字段（I 不覆盖的那些） ---
	credentialType   credential.Type
	stripImageTools  bool
	modelsDigest     [32]byte
	formatModelsKey  [32]byte
	supportedFormats [32]byte
	modelMappingKey  [32]byte

	// --- §5.5 group_ids 行（Slice 类事实以规范序摘要入键） ---
	groupIDsDigest [32]byte
}

// payloadKey 是只影响"叶子内容是否新鲜"的载荷：OAuth 访问令牌三元组只用于
// 上游鉴权（stableCredentialDigest 对 codex-oauth 返回常量，token/refresh/
// expiry 完全不进指纹），不是任何路由决策的输入。载荷变化必须换新叶子
// （SDK 拿到新 token，staticKey 变）但不得作废在途计划（planKey 不变）。
type payloadKey struct {
	codexOAuthToken   string // 凭据值：上游鉴权访问令牌
	codexOAuthRefresh string // 凭据值：刷新令牌
	codexOAuthExpires int64  // 凭据值：访问令牌过期时刻（UnixNano；0 = 未设置）
}

// staticKey 是快照静态事实的**可比较值类型**（spec §5.5b「结构保证」）：
// sameStatic := staticKeyOf(old) == staticKeyOf(new)，算子是 `==`。
//
// 叶子复用判据 = 决策输入 ∪ 载荷：凭据值（含 OAuth token）必须入键——否则
// 凭据轮转后键判等 ⇒ 复用旧叶 ⇒ 上游拿旧令牌 401（实测：
// TestInvalidateAccountReloadsExt 失败）。而「token 刷新不该改计划有效性」
// 由 planKey 承担：两件事不共用一个比较。
//
// §5.5b 比较规则第 6 项（快照容器完备性）：snapshotStatic 的每个字段都落入键或由
// 已比较项派生——acc → 账号侧行；tpl → 模板侧行；groupIDs → group_ids 行；
// gid → min(groupIDs) 纯派生（见 buildSnapshots）。
//
// **本类型必须不含任何指针/切片/映射字段**：含指针的 struct 仍然「可比较」，
// 但 `==` 对指针只比**地址**；键是每次重载重建的（buildSnapshots 每次 new
// snapshotStatic），只要键里有一个指向「该次重载新建对象」的指针（state.go 的
// tpl/Ext，routing_compiler_candidates.go:70-71 内嵌的 account/static），键就
// **恒不等** ⇒ 规则重新变惰性——即本 spec 要消灭的那个缺陷。Slice/Map 类事实一律
// 以**规范序 32 字节摘要**（[32]byte，kind = reflect.Array，保持 struct 可比较）入键。
// 守卫见 static_key_test.go。
type staticKey struct {
	planKey
	payloadKey
}

// planKeyOf 由快照静态视图派生计划有效性判据。av 为 nil（首轮无旧快照）时
// 返回零值键；调用方不得依赖零值比较做缺席判定——fence 必须显式处理缺席。
//
// 生效 baseURL 与编译器同源（routing_compiler_candidates.go:98-103）：模板值为底，
// 账号覆盖非空时优先。两者必须是同一套优先级，否则判据会在编译器认为「变化了」的
// 场景下判等（或反之），复用分支与编译事实就此分叉。
func planKeyOf(av *snapshotStatic) planKey {
	var k planKey
	if av == nil {
		return k
	}
	k.accountID = av.acc.ID
	k.name = av.acc.Name
	k.templateID = av.acc.TemplateID
	if av.tpl != nil {
		k.baseURL = av.tpl.BaseURL
	}
	if av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
		k.baseURL = *av.acc.BaseURL
	}
	k.upstreamKey = av.acc.UpstreamKey
	k.maxConcurrency = av.acc.MaxConcurrency
	k.enabled = av.acc.Enabled
	k.cacheDomain = normalizeCacheDomain(av.acc.CacheDomain)
	k.upstreamCostMultiplierBp = av.acc.UpstreamCostMultiplierBp
	k.identityRevision = av.acc.IdentityRevision
	if ext := av.acc.Ext; ext != nil {
		k.codexAccountID = derefString(ext.CodexAccountID)
		k.codexEmail = derefString(ext.CodexEmail)
		k.codexPATKey = derefString(ext.CodexPATKey)
		if id := ext.CodexIdentity; id != nil {
			k.codexInstallation = id.InstallationID
			k.codexSession = id.SessionID
			k.codexThread = id.ThreadID
			k.codexWindow = id.WindowID
		}
	}
	if tpl := av.tpl; tpl != nil {
		k.credentialType = tpl.CredentialType
		k.stripImageTools = tpl.StripImageTools
		k.modelsDigest = digestStrings(tpl.Models)
		k.formatModelsKey = digestFormatModels(tpl.FormatModels)
		k.supportedFormats = digestFormats(tpl.SupportedFormats)
		k.modelMappingKey = digestModelMapping(tpl.ModelMapping)
	}
	k.groupIDsDigest = digestInt64s(av.groupIDs)
	return k
}

// payloadKeyOf 由快照静态视图派生载荷判据：只读 OAuth 令牌三元组。av 为 nil
// 时返回零值。
func payloadKeyOf(av *snapshotStatic) payloadKey {
	var k payloadKey
	if av == nil {
		return k
	}
	if ext := av.acc.Ext; ext != nil {
		k.codexOAuthToken = derefString(ext.CodexOAuthToken)
		k.codexOAuthRefresh = derefString(ext.CodexOAuthRefreshToken)
		if ext.CodexOAuthExpiresAt != nil {
			k.codexOAuthExpires = ext.CodexOAuthExpiresAt.UnixNano()
		}
	}
	return k
}

// staticKeyOf 由快照静态视图派生比较键。av 为 nil（首轮无旧快照）时返回零值键；
// 零值键只在两侧都为空时相等（首轮无复用分支，oldByID 为空 map）。
//
// 两枚子键从**同一份**快照派生，字段归属只声明一次：planKeyOf 读决策输入，
// payloadKeyOf 读载荷。
func staticKeyOf(av *snapshotStatic) staticKey {
	return staticKey{planKey: planKeyOf(av), payloadKey: payloadKeyOf(av)}
}

// normalizeCacheDomain 把 nil 与 "" 归并为同一个空串：cacheDomainForAccount
// （cache_affinity.go:100-105）对二者都回私有域，消费者等价 ⇒ 键不得制造比
// 消费面更细的差异。
func normalizeCacheDomain(d *string) string {
	if d == nil {
		return ""
	}
	return *d
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// digestStrings 对字符串列表取规范序摘要（先排序，再长度前缀串联，最后 sha256）。
func digestStrings(items []string) [32]byte {
	cp := append([]string(nil), items...)
	sort.Strings(cp)
	return digestParts(len(cp), func(i int) []byte { return []byte(cp[i]) })
}

// digestInt64s 对 int64 列表取规范序摘要（排序 + 长度前缀串联 + sha256）。
func digestInt64s(items []int64) [32]byte {
	cp := append([]int64(nil), items...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	var buf [8]byte
	return digestParts(len(cp), func(i int) []byte {
		binary.BigEndian.PutUint64(buf[:], uint64(cp[i]))
		return buf[:]
	})
}

// digestFormats 对格式列表取规范序摘要（排序按字符串序，与域内 SupportedFormats
// 的元素语义一致）。
func digestFormats(items []domain.RequestFormat) [32]byte {
	cp := append([]domain.RequestFormat(nil), items...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return digestParts(len(cp), func(i int) []byte { return []byte(cp[i]) })
}

// digestFormatModels 对 format → 模型列表映射取规范序摘要：键排序，值列表各自
// 排序，键与每个元素都带长度前缀，防「键值边界」歧义。
func digestFormatModels(m map[domain.RequestFormat][]string) [32]byte {
	if len(m) == 0 {
		return digestParts(0, nil)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	// 每个键固定贡献「键 + 值个数」两个 part，每个值贡献一个 part；预先算好容量。
	n := 0
	for _, k := range keys {
		n += 2 + len(m[domain.RequestFormat(k)])
	}
	parts := make([][]byte, 0, n)
	for _, k := range keys {
		parts = append(parts, []byte(k))
		vals := append([]string(nil), m[domain.RequestFormat(k)]...)
		sort.Strings(vals)
		parts = append(parts, []byte(itoa(len(vals))))
		for _, v := range vals {
			parts = append(parts, []byte(v))
		}
	}
	return digestParts(len(parts), func(i int) []byte { return parts[i] })
}

// digestModelMapping 对模型映射取规范序摘要（键排序；条目的映射目标与模式都入摘要）。
func digestModelMapping(m domain.ModelMapping) [32]byte {
	if len(m) == 0 {
		return digestParts(0, nil)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([][]byte, 0, len(keys)*3)
	for _, k := range keys {
		e := m[k]
		parts = append(parts, []byte(k), []byte(e.MappedModel), []byte(itoa(int(e.Mode))))
	}
	return digestParts(len(parts), func(i int) []byte { return parts[i] })
}

// digestParts 把 parts（个数 n）按「长度前缀 + 内容」串联后 sha256。n 也计入摘要，
// 8 字节大端长度前缀消除拼接歧义（["ab","c"] 与 ["a","bc"] 摘要不同）。
func digestParts(n int, part func(i int) []byte) [32]byte {
	h := sha256.New()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(n))
	h.Write(lenBuf[:])
	for i := 0; i < n; i++ {
		p := part(i)
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		h.Write(lenBuf[:])
		h.Write(p)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// itoa 是 strconv.Itoa 的本地零依赖替代（摘要热路径，避免引入 strconv）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
