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

// staticKey 是快照静态事实的**可比较值类型**（spec §5.5b「结构保证」/A2①）：
// sameStatic := staticKeyOf(old) == staticKeyOf(new)，算子是 `==`。
//
// 字段集 = spec §5.5 表「静态快照消费 = 是」的账号侧行 ∪ §5.5b 的模板/凭据消费子集：
//
//	§5.5 账号侧：name / template_id / base_url（生效值）/ upstream_key /
//	  max_concurrency / group_ids / enabled / cache_domain / upstream_cost_multiplier /
//	  ext 凭据面消费子集 / identity_revision（K）
//	§5.5b 模板侧：templateBaseURL / credentialType / stripImageTools /
//	  models / formatModels / supportedFormats / modelMapping
//	§5.5b 凭据侧：codexAccountID / codexIdentity
//
// 比较规则第 2 项（`CandidateFingerprint` 本身按值比较）在本批**未接线**：
// 它按值覆盖 accountID/templateID/credential_type/生效 baseURL/stripImageTools/
// upstreamKey/凭据摘要/凭据身份四元组/codexAccountID，这些事实**均已逐项**落在上列
// 字段里（digest 是 upstreamKey|patKey 的纯函数：domain/routing.go:231-257），故键
// 覆盖面无缺口；接线它属 spec §5.7(b)「sameStatic 可达性修复」项，与 (A) 合并
// 两类型同批（本批显式排除）。
//
// §5.5b 比较规则第 6 项（快照容器完备性）：snapshotStatic 的每个字段都落入键或由
// 已比较项派生——acc → 上列账号侧行；tpl → 上列模板侧行；groupIDs → group_ids 行；
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

	// --- 凭据面消费子集 ---
	//
	// 这里入键的是**叶上被消费的凭据值**，不是「身份」。区分是承重的：
	// staticKey 的消费者是 hasStaticChange（attempt_plan_exec.go:378 比较
	// **叶指针**）⇒ 键的语义是「叶上被消费的事实是否变了」：变了就换新叶，
	// 在途 plan 跳过该账号（spec §5.7(b)「态 2 ⇒ 预留成功」）；未变的账号
	// 复用旧叶、可继续预留。
	//
	// 故判据是 A2 的「消费面 ⊆ 键字段集」，不是「身份」。上游鉴权要用的
	// 凭据是从叶消费的（invalidate_account_test.go:40 断言回写后叶上 Ext 必须
	// 换新），所以凭据值**必须入键**——否则凭据轮转后键判等 ⇒ 复用旧叶 ⇒
	// 上游拿旧令牌 401（实测：TestInvalidateAccountReloadsExt 失败）。
	//
	// 「token 刷新不该改身份」由**另一套机制**承担：(I,K) 围栏（latch/health/
	// continuation 直接比较 K）。两件事不得共用一个比较——
	// 这正是本键与 (I,K) 必须分开的原因。
	codexAccountID    string
	codexInstallation string
	codexSession      string
	codexThread       string
	codexWindow       string
	codexEmail        string // 叶上 Ext 经 Selection.Ext 可达，故入键保新鲜
	codexPATKey       string // 凭据值：pat（api_key/pat 类型上游鉴权用）
	codexOAuthToken   string // 凭据值：上游鉴权访问令牌
	codexOAuthRefresh string // 凭据值：刷新令牌
	codexOAuthExpires int64  // 凭据值：访问令牌过期时刻（UnixNano；0 = 未设置）

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

// staticKeyOf 由快照静态视图派生比较键。av 为 nil（首轮无旧快照）时返回零值键；
// 零值键只在两侧都为空时相等（首轮无复用分支，oldByID 为空 map）。
//
// 生效 baseURL 与编译器同源（routing_compiler_candidates.go:98-103）：模板值为底，
// 账号覆盖非空时优先。两者必须是同一套优先级，否则键会在编译器认为「变化了」的
// 场景下判等（或反之），复用分支与编译事实就此分叉。
func staticKeyOf(av *snapshotStatic) staticKey {
	var k staticKey
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
		k.codexOAuthToken = derefString(ext.CodexOAuthToken)
		k.codexOAuthRefresh = derefString(ext.CodexOAuthRefreshToken)
		if ext.CodexOAuthExpiresAt != nil {
			k.codexOAuthExpires = ext.CodexOAuthExpiresAt.UnixNano()
		}
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
