// Package architecture 是仓库级 import-graph 门禁：本目录只有测试文件，不产出任何
// 生产代码，只在 go test 时读取真实 import 图。
//
// # 三类保证，强度完全不同（不要混为一谈）
//
// ① 三条硬规则 —— 机械强制：
//
//  1. pkg/* 不得 import 任何 internal/*（零豁免，连 legacy 都登记不进去）；
//  2. 未显式声明层级的包落到 leafRule，默认只允许 domain/credential/worker/pkg，
//     也就是「未知包默认拒绝」；
//  3. allow 矩阵之外的边默认拒绝：任何不在 allow 里的边，只能显式登记成债务，
//     否则失败。
//
// ② legacy 债务账本 —— 机械部分 + 人工部分：
//
//   - 机械：stale 检查（登记了但图上没有 / 已被 allow 覆盖都要红）与条数精确相等
//     （len(debt) == debtMax，删一条必须同步改小）；
//   - 人工：「新边 + 当场登记 legacy{reason, phase} + 同步调大上限」这三步合起来
//     在机械上完全合法，账本无法区分「偿还债务」与「偷偷加债」，只能靠 diff 复核。
//     账本本身不会让代码变好，它只防止债务悄悄变大。
//
// ③ allow 矩阵 —— 指纹 tripwire（机械信号 + 评审），不是硬规则：
//
//   - allowMatrixFingerprint 是整张策略（allowRules + leafRule）规范化序列化后的
//     sha256：规则按 from 排序，all / to / deny 各自排序后按固定字段顺序拼接。
//     因此任何矩阵改动——加一条 from、加宽一条 to、多一个 deny、把 to 换成 all、
//     动 leafRule——都会让指纹变化，必须先显式更新 const 才能过测试；
//
//   - allMax 单独限制 all: true 的条数（精确相等，只许改小），因为 all: true 是
//     最危险的改动方向：一条规则等于整棵子树立即可依赖；
//
//   - 两者互补：指纹抓「任何矩阵改动」，allMax 抓「最危险的改动方向」。没有指纹时
//     放宽矩阵是零信号（allowRule 没有 reason / phase / 上限，比 legacy 还宽松）；
//     这正是本 tripwire 存在的理由。
//
//     但必须诚实：没有任何机制能阻止有意同时改矩阵与指纹。门禁的作用是让这类改动
//     不可静默——它必须同时出现在 diff 的矩阵与指纹（必要时还有 allMax）两处，评审
//     看得见理由，而不是不可发生。
//
// # 本门禁挡不住什么
//
// 它只看包级 import 边，因此挡不住：
//   - 同包内的层次倒挂（同一个 package 里 controller 逻辑直接写 SQL）；
//   - 类型谓词泄漏（例如 scheduler 内部按 account.IsCodex() 分流执行细节）。
//
// 这些是上游 S0/S2 的 AST 门禁职责，不要指望本文件提供那种保证。
//
// # 迁移期流程
//
//   - 重命名 / 拆包时，旧边必须在同一个提交里从账本删除（否则 stale 会红）；
//   - 新边若确属既有债务，必须单独提交并单写理由，不许在「结构迁移」提交里
//     顺手放宽 allow 矩阵或调大上限；
//   - phaseKeep（结构性不消除）单独计上限，不许用它给新边开口子；
//   - 搬包（P3/P5）时新树只能写 to: 白名单，不许用 all: true。
//
// # 扫描口径
//
// go list ./internal/... ./cmd/... ./pkg/... ./tools/... ./demo/...（只为真实存在的
// 顶层目录生成 pattern），一次子进程、进程内缓存；GOFLAGS 被强制清空，避免外部
// -tags 改变包集。包集必须与文件系统遍历结果（减去 unscannedDirs）双向相等：
// 少扫、多扫都要显式说明。Imports 走严格矩阵，TestImports / XTestImports 走
// 更宽口径，并各自维护独立账本。
//
// 文件系统遍历与 go list 同一口径：testdata / tmp / vendor / node_modules 按
// basename 在任意深度都不深入（Go 工具链同样忽略 testdata；vendor 是依赖副本；
// tmp 是临时目录；node_modules 不装 Go 包）。根级的 testdata / tmp / vendor 仍要
// 在 unscannedDirs 里写明「为什么根级这些目录不被扫」，其余深度既不深入也不登记，
// 否则每个叶子包的 testdata 夹具都会变成一条假失败。
package architecture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	modulePath = "github.com/is7qin/c3api/"

	// scanTimeout 只是「别把测试挂死」的兜底；性能指标是 scanWarnBudget。
	// 只 log 不 assert：冷 GOCACHE 下 go list 可能到秒级，硬阈值会变成 flake 源。
	scanTimeout    = 60 * time.Second
	scanWarnBudget = 2 * time.Second

	// minPackages 是防止空图的廉价下限（空图会让所有断言无条件通过）。
	// 89 是基 commit(7a486270) 的包数；当前口径实测 93（含本门禁与 3 个 tools 包）。
	// 真正的防漏机制是 TestImportGraphScanCoverage 的双向包集等价。
	minPackages = 89
)

// ---------------------------------------------------------------------------
// 图与规则的基本类型
// ---------------------------------------------------------------------------

// edge 是一条精确的包级 import 边，两端都去掉了 modulePath 前缀
// （例如 internal/handler/user -> internal/repository）。
type edge struct {
	from string
	to   string
}

func (e edge) String() string { return e.from + " -> " + e.to }

// allowRule 描述「from 前缀命中的包允许 import 哪些 to 前缀」。
// 同一 from 命中多条规则时取最长前缀，保证精确包优先于顶层目录。
type allowRule struct {
	from string
	all  bool     // 不限制 to（装配点 / 执行面 / 消费者入口）
	to   []string // to 的前缀白名单；"internal/service" 同时覆盖 internal/service/errors
	deny []string // 优先于 all/to 的否决项（目前只有 service -> proxy）
}

// policy 是一份完整的判定矩阵：显式规则 + 未声明包落到哪里。
type policy struct {
	rules    []allowRule
	fallback allowRule
}

// judgement 带上是哪条规则判的，失败信息要能直接指出改哪里。
type judgement struct {
	allowed bool
	rule    string
}

func (p policy) ruleFor(pkg string) (allowRule, bool) {
	var best allowRule
	found := false
	for _, r := range p.rules {
		if !matchesPrefix(pkg, r.from) {
			continue
		}
		if !found || len(r.from) > len(best.from) {
			best, found = r, true
		}
	}
	return best, found
}

func (p policy) judge(e edge) judgement {
	if strings.HasPrefix(e.from, "pkg/") {
		if strings.HasPrefix(e.to, "internal/") {
			return judgement{rule: "硬规则：pkg/* 不得 import internal/*"}
		}
		return judgement{allowed: true, rule: "pkg/* 内部依赖"}
	}
	if e.from == e.to {
		// 外部测试包（package xxx_test）import 自己被测的包，是同一条边，不构成跨层。
		return judgement{allowed: true, rule: "包引用自身"}
	}
	if isEntPackage(e.from) && isEntPackage(e.to) {
		return judgement{allowed: true, rule: "ent 生成代码内部自引用不判"}
	}

	r, ok := p.ruleFor(e.from)
	if !ok {
		r = p.fallback
	}
	for _, d := range r.deny {
		if matchesPrefix(e.to, d) {
			return judgement{rule: fmt.Sprintf("规则 %q 显式禁止 %q", r.from, d)}
		}
	}
	if r.all {
		return judgement{allowed: true, rule: fmt.Sprintf("规则 %q 全允许", r.from)}
	}
	if matchesAnyPrefix(e.to, r.to) {
		return judgement{allowed: true, rule: fmt.Sprintf("规则 %q 允许", r.from)}
	}
	return judgement{rule: fmt.Sprintf("规则 %q 只允许 [%s]", r.from, strings.Join(r.to, ", "))}
}

// widened 返回「更宽」的测试侧矩阵：每层都可额外 import 夹具包。
func (p policy) widened(extra []string) policy {
	w := policy{fallback: p.fallback, rules: make([]allowRule, 0, len(p.rules))}
	w.fallback.to = append(append([]string(nil), p.fallback.to...), extra...)
	for _, r := range p.rules {
		r.to = append(append([]string(nil), r.to...), extra...)
		w.rules = append(w.rules, r)
	}
	return w
}

func isEntPackage(pkg string) bool {
	return pkg == "internal/ent" || strings.HasPrefix(pkg, "internal/ent/")
}

func isHardRuleEdge(e edge) bool {
	return strings.HasPrefix(e.from, "pkg/") && strings.HasPrefix(e.to, "internal/")
}

// matchesPrefix 既支持「整包」，也支持显式以 "/" 结尾的子树前缀：
// internal/handler 命中 internal/handler/user，但 internal/domain 不命中 internal/domainx。
func matchesPrefix(pkg, prefix string) bool {
	if prefix == "" {
		return false
	}
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(pkg, prefix)
	}
	return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
}

func matchesAnyPrefix(pkg string, prefixes []string) bool {
	for _, p := range prefixes {
		if matchesPrefix(pkg, p) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 目标允许边（P3–P6 重命名后再更新此表）
// ---------------------------------------------------------------------------

// leafRule 是「未显式声明的包」的兜底：只允许依赖最底层的 domain/credential/worker
// 与 pkg/*。这是硬规则 2（未知包默认拒绝）：新包不会因为「没写规则」而自动放行。
var leafRule = allowRule{
	from: "<未显式声明：叶子层>",
	to:   []string{"internal/domain", "internal/credential", "internal/worker", "pkg/"},
}

var allowRules = []allowRule{
	// 唯一装配点：允许依赖全部（cmd/ 下只允许存在它，见 TestImportGraphSingleCompositionRoot）。
	{from: "cmd/server", all: true},

	// 开发 / 压测 / 假上游 / 演示种子等消费者入口：不属于运行期分层，
	// 但一律纳入扫描，不允许静默漏包。
	{from: "tools/", all: true},
	{from: "demo/", all: true},

	// 门禁自身：只有 _test.go，不依赖任何 internal 包。
	{from: "internal/architecture"},

	// 执行面：唯一持有凭据 / 网络 / 执行实现的层。数据面禁止反向依赖传输/控制器层：
	// handler/server 是传输壳，service 已经反过来 deny 了 proxy（这对方向是刻意隔开的）。
	{from: "internal/proxy", all: true, deny: []string{"internal/handler", "internal/server", "internal/service"}},

	// 控制器层：除了直接摸执行面（proxy）之外基本允许。
	{from: "internal/service", all: true, deny: []string{"internal/proxy"}},
	{from: "internal/service/epay", to: []string{"internal/domain"}},
	{from: "internal/service/errors", to: []string{"internal/domain"}},

	// 纯类型层：只依赖凭据模型。
	{from: "internal/credential"},
	// pkg/egressapi 是出口值类型与归一化（Spec / Key / New）的**中立叶子包**：domain
	// 的 Proxy.EgressSpec 要造一个规格值，凭此不再 import 实现层 pkg/egress（账本
	// 原有的那条 P3 债因此消失）。方向固定：egressapi 只 import stdlib，绝不反向。
	{from: "internal/domain", to: []string{"internal/credential", "pkg/egressapi"}},

	// 生成代码：只允许自引用。ent 模型 / schema 复用 domain 枚举属于 Codegen 边界
	// 尚未抽离的债务，登记在 legacy(P3)；绝不把整棵生成代码树做成永久豁免。
	{from: "internal/ent", to: []string{"internal/ent/"}},

	// 存储层：允许 ent 与 domain/credential。
	//
	// P5-1 起多一条 internal/loadport：装载端口（GroupAccountBatch / BatchedGroupLoader
	// / ExtCredentialMismatch）住在中立叶子包里，repository 作为**实现方**引用契约。
	// 方向是"实现 → 契约"：loadport 只依赖 domain + credential，绝不反向 import 本包，
	// 故这条边不构成环。这是本单新增的第 1 条 allow 边。
	{from: "internal/repository", to: []string{
		"internal/domain", "internal/credential", "internal/ent", "internal/loadport",
	}},

	// 中立契约层（P3b）：数据面/控制面共享的 Selection 结构 + 4 方法句柄 + 2 方法计划面。
	// 只依赖值类型层与上游 SDK 桥的**值类型**（domain/credential/sdkbridge.ThreadHandle）；
	// F2 的 import 图半边：**绝不许** import internal/scheduler（会构成契约环）。
	// pkg/ 与 leafRule 同口径显式登记：唯一实际用到的是 pkg/egress.Client 指针
	// （类型随账号快照携带，api 不调实现——把实现挡在数据面/控制面之外）。
	{from: "internal/api", to: []string{
		"internal/domain", "internal/credential", "internal/sdkbridge", "pkg/",
	}},

	// 装载端口（P5-1）：控制面 ↔ 存储实现之间的中立契约——批次会话（同一模板在整批内
	// 只有一枚 *domain.Template）与被跳过的受损账号这条装载期观测。
	// 只依赖值类型层；**不**依赖 repository（实现方要 import 它，反向成环）、**不**依赖
	// scheduler（能力由消费方描述，实现方无需 import 它）。选它而不是 internal/api 的原因
	// 写在包注释里：api 自己 import sdkbridge，而 sdkbridge import repository。
	{from: "internal/loadport", to: []string{"internal/domain", "internal/credential"}},

	// 身份围栏（P5-1，R2 的 D5）：候选指纹与三个哨兵的 canonical 定义 + 归属门禁。
	// 只依赖 domain（指纹由 domain.AccountCandidateFingerprint / CandidateFPHex 派生）；
	// 不依赖 repository（ErrStaleIdentityRevision 是仓储 CAS 的哨兵，归 repository）、
	// 不依赖 scheduler（否则等于把哨兵换了个地方住）。
	{from: "internal/identityfence", to: []string{"internal/domain"}},

	// 规则判定落库（P5-1）：从 scheduler 迁出，只改包声明 + 4 个限定符，其余逐字节不变。
	// 它自己**不持哨兵**（canonical 在 identityfence），并且**不** import scheduler
	// （否则"迁出"只是挪了一层，且 scheduler 侧的兼容层会与它成环）。
	// 两条 to 元素的代价必须写在提交正文：repository 是**实现依赖**（落库 CAS 的
	// ErrStaleIdentityRevision），identityfence 是**契约依赖**（三个哨兵 + 指纹）——
	// 这是本单新增的第 3、4 条 allow 边。
	{from: "internal/rulepersist", to: []string{
		"internal/domain", "internal/identityfence", "internal/latch",
		"internal/repository", "internal/rule", "pkg/logx",
	}},

	// 身份池（P5b）：会话池的唯一职责是持有"线程生命周期"，因此它依赖上游 SDK 桥的
	// **线程机**（sdkbridge.ThreadHandle / Threads）；除此之外只有 stdlib。轮换**策略**
	// 自 P5-2b 起归本包所有（Policy / DefaultPolicy），SDK 桥只把它当构造参数收——
	// 这条边不再为"策略"而存在（P5-2b 收窄的是理由，规则与指纹不变）。
	// 它**不知道** scheduler 的任何类型（accountConfig / egress.Client / 资源批次），
	// 反向边由 TestImportGraphProduction 的 allow 矩阵直接拒绝——这条规则是 P5b 那单
	// 唯一新增的边，指纹随之显式更新（加宽必须可见）。
	{from: "internal/identitypool", to: []string{"internal/sdkbridge"}},

	// 调度：只依赖值类型层 + worker；执行面细节按 P5 收口（见账本）。
	// P3b 起多一条 internal/api：控制面把 Selection 别名为契约类型、并实现 SelectionHandle。
	// P5b 起多一条 internal/identitypool：codex 会话池下沉为独立包（只搬"线程生命
	// 周期"，不搬配置/出口），scheduler 经 Spec/Claim/Reclaimer 三处窄缝用它。
	// P5-1 起多两条，都是"把实现从核心路由里挪走"的直接结果：
	//   - internal/loadport：批次会话契约（装载端口）——scheduler 只持**会话**，作用域
	//     在实现包内，故它不再需要 internal/repository（账本 574 那条边消失）；
	//   - internal/identityfence：候选指纹与三个哨兵的 canonical 定义。scheduler 只留
	//     迁移期兼容层（同名别名 + 一行转发），哨兵必须是同一 error 值。
	// P5-3-A 起多一条 pkg/egressapi：出口的**值类型**（Spec / Client / Flavour /
	// ResourceKey 及其全部方法）已迁到该中立包，快照、静态键与资源键只认它。
	// 它与 deny 里的 "pkg/egress" 是**两个不同的包**：deny 走段感知前缀
	// （matchesPrefix：`pkg == prefix || HasPrefix(pkg, prefix+"/")`），所以
	// pkg/egressapi 不会被 "pkg/egress" 误伤——落地前实测确认（本包用例绿）。
	//
	// P5-3-B 起 deny 变成**硬规则**：调度器声明消费方端口 EgressPool（四个方法），
	// Config.Egress 改接口，egress.NewPool(nil) / CloseClients 全部退场——本包不再
	// 把出口**实现**当作自己类型系统的一部分（同时删掉账本 576）。
	// 诚实边界：这只消除**直接边**。pkg/egress 仍在 scheduler 的**传递**闭包里
	// （唯一路径 scheduler → sdkbridge → pkg/egress，债 575 未清）——sdkbridge 是
	// 真正把字节送上网的执行面，它持有实现是设计而不是债。别拿
	// `go list -deps ./internal/scheduler` 来"证明"这条债没修好。
	{from: "internal/scheduler", to: []string{
		"internal/domain", "internal/credential", "internal/worker", "internal/api",
		"internal/latch", "internal/rule", "internal/identitypool", "pkg/logx",
		"internal/loadport", "internal/identityfence", "pkg/egressapi",
	}, deny: []string{"pkg/egress"}},

	// 上游 SDK 桥：执行面的一端，允许 pkg/egress 实现。
	// P3c 起新增一条 internal/identityfence：三个身份围栏哨兵不再由本包各铸一枚
	// errors.New（那会让跨包 errors.Is 恒假，上游已分类成"缺身份"的判决落到本包会
	// 被 isTransientFailure 判成"可重试"），而是 canonical 的变量别名（同一 error 值）。
	// 方向是"执行面实现 → 叶子契约"：identityfence 只 import domain，既不成环，也不会把
	// repository/scheduler 带进来。
	{from: "internal/sdkbridge", to: []string{
		"internal/domain", "internal/credential", "internal/identityfence", "internal/worker",
		"pkg/egress", "pkg/logx",
	}},

	// HTTP 控制器：自己的子包 + 控制器层，不允许直接摸 repository/scheduler，
	// 也不允许反向依赖传输壳 server（那是一条 P3 债务，见账本）。
	{from: "internal/handler", to: []string{
		"internal/handler/", "internal/domain", "internal/credential",
		"internal/service", "internal/auth", "pkg/logx",
	}},

	// 传输壳：只依赖 domain 与两个 pkg 原语。
	{from: "internal/server", to: []string{"internal/domain", "pkg/logx", "pkg/sserelay"}},

	// 测试支撑：允许 db + repository。
	{from: "internal/testsupport", to: []string{"internal/db", "internal/repository", "internal/testsupport/"}},

	// 以下为支撑/叶子包：逐包声明，避免任何包悄悄拿到 repository/scheduler。
	{from: "internal/auth", to: []string{"internal/domain"}},
	{from: "internal/billing", to: []string{"internal/domain", "internal/worker", "pkg/logx"}},
	{from: "internal/config", to: []string{"internal/domain"}},
	{from: "internal/continuation", to: []string{"internal/domain"}},
	{from: "internal/discovery", to: []string{"internal/worker", "pkg/logx"}},
	{from: "internal/invalidate", to: []string{"internal/worker", "pkg/logx"}},
	{from: "internal/latch", to: []string{"internal/rule"}},
	{from: "internal/notification", to: []string{"internal/domain", "internal/worker", "pkg/logx"}},
	{from: "internal/notify", to: []string{"internal/worker", "pkg/logx"}},
	{from: "internal/paymentregistry", to: []string{"internal/domain", "internal/service/epay"}},
	{from: "internal/pricing", to: []string{"internal/domain", "internal/service/errors", "internal/worker", "pkg/logx"}},

	// 探测面：需要 sdkbridge 客户端做健康探测，属执行面延伸。
	{from: "internal/probe", to: []string{
		"internal/credential", "internal/domain", "internal/sdkbridge", "pkg/aiclient", "pkg/egress",
	}},

	{from: "internal/protoconv", to: []string{"internal/domain", "pkg/sserelay"}},
	{from: "internal/quality", to: []string{"internal/domain", "internal/worker", "pkg/logx"}},
	{from: "internal/rule", to: []string{"internal/domain", "internal/worker", "pkg/logx"}},
	{from: "internal/settingssnap", to: []string{"internal/domain", "pkg/logx"}},
	{from: "internal/supplier", to: []string{"internal/domain", "internal/worker", "pkg/logx"}},
	{from: "internal/usage", to: []string{"internal/domain", "internal/worker", "pkg/logx"}},
	{from: "internal/verification", to: []string{"internal/domain"}},
	{from: "internal/worker", to: []string{"pkg/logx"}},

	// 存储实现层：把 ent + repository 组装成读写门面（当前分支无此包，合并后生效）。
	{from: "internal/db", to: []string{
		"internal/domain", "internal/credential", "internal/ent",
		"internal/repository", "internal/db/", "pkg/",
	}},

	// 迁移工具链：自引用 + 生成器专用边。
	{from: "internal/db/migrate/gen/cmd", to: []string{"internal/db/migrate/gen"}},
	{from: "internal/db/migrate/gen", to: []string{"internal/db/migrate/catalog", "internal/ent/migrate"}},
	{from: "internal/db/migrate/catalog"},
	{from: "internal/db/migrate", to: []string{"internal/db/migrate/"}},

	{from: "internal/snapshot"},
}

// testFixtureAllow 是测试侧的额外放宽：测试允许跨层构造夹具（数据库、ent 实体、
// testsupport 骨架、任意 pkg 原语）。它只影响 TestImports / XTestImports 的裁决。
var testFixtureAllow = []string{
	"internal/testsupport/",
	"internal/ent/",
	"internal/db/migrate",
	"internal/repository",
	// P5-1：scheduler 的两处用例要构造**真实** persist 函数——PG 用例验证"ext 投影失败
	// 不得挡住判决落库"（account_ext_mismatch_pg_test.go），兼容层用例验证陈旧 K 的传播
	// 与锁存清除（candidate_identity_test.go）。用例的主语是 scheduler（脏行装载 + 快照
	// + latch 行为），不能搬走。生产边仍由 productionLedger 管——scheduler 的**生产**
	// 代码没有这条 import（见 internal/scheduler 的规则）。
	"internal/rulepersist",
	// P5-4：scheduler 的两条用例要构造**真实**的 codex 传输 adapter——
	// reconcile_removal_order_test.go 与 compile_quiet_reclaim_test.go 断言的是
	// "资源回收/静默 reclaim 的顺序与退役时机"，必须让 adapter.Prepare 真的走一遍
	// 传输路径才成立；换成端口替身等于把用例改成测替身。
	//
	// 生产边仍由 productionLedger 管：scheduler 的**生产**代码没有这条 import
	// （P5-4 已把它从 scheduler 的 allow to 列表里删掉，testFixtureAllow 只加宽
	// TestImports 口径，不影响 Imports 口径）。
	"internal/sdkbridge",
	"pkg/",
}

func productionPolicy() policy {
	return policy{rules: allowRules, fallback: leafRule}
}

// ---------------------------------------------------------------------------
// allow 矩阵指纹（加宽的唯一机械信号）
// ---------------------------------------------------------------------------

const (
	// allowMatrixFingerprint 钉住整张策略（allowRules + leafRule）的规范化 sha256。
	//
	// 背景：allowRule 没有 reason / phase / 上限，而 legacyRow 三者都有——放宽
	// 矩阵比增加债务还便宜。指纹把「放宽矩阵」变成一次不可静默的改动：矩阵变了
	// 指纹就变，必须先显式更新本常量才能过测试。
	//
	// 诚实边界：它挡不住有意同时改矩阵与指纹。它只保证这类改动在 diff 里两处同时
	// 可见、必须写理由（见文件头 ③），不保证它不会发生。
	// P5-1 变更（4 条新增 allow 边，逐条见 allowRules 的注释；全部是 to: 白名单，
	// 没有新增 all: true）：
	//   1. repository 的 to 加 internal/loadport（实现 → 装载端口契约）
	//   2. scheduler 的 to 加 internal/loadport（消费方 → 装载端口契约）
	//   3. scheduler 的 to 加 internal/identityfence（迁移期兼容层的别名主语）
	//   4. 新规则 internal/rulepersist → {domain, identityfence, latch, repository, rule, pkg/logx}
	//      （这一条含 2 条边：到 identityfence 的契约依赖 + 到 repository 的实现依赖）
	// 同时新增两条叶子规则 internal/loadport 与 internal/identityfence。
	// P5-3-A 变更（1 条新增 allow 边，仍只有 to: 白名单，没有新增 all: true）：
	//   scheduler 的 to 加 pkg/egressapi（出口值类型的中立落点）。
	// P5-3-B 变更：**指纹不变，这是预期结果**。本单删的是账本里的一行（576），
	//   allow 矩阵一个字节都没动——scheduler 的 to 与 deny 原样保留（deny 从此是
	//   硬规则），也没有新增 all: true。指纹钉的是 allowRules + leafRule 的规范化
	//   序列化，不含账本，所以"删债"不该动它；反过来，若这里被迫改指纹，就说明
	//   有人在删债的同时动了矩阵——那正是本常量存在的理由。
	// 债 s1-a-class 变更（1 条新增 allow 边，仍是 to: 白名单，没有新增 all: true）：
	//   internal/domain 的 to 加 pkg/egressapi。**格局与 P5-3-B 相反，必须说清**：
	//   本单确实同时动了矩阵（不是"删债时偷动矩阵"），原因是 domain 的
	//   Proxy.EgressSpec 要造出口规格值，而归一化 New 已随 Spec 归入 egressapi。
	//   新增这条边换来的是账本里 domain → pkg/egress 那条 P3 债的**消失**：
	//   依赖从实现层（连接池/transport 装配）降到了只含 stdlib 的中立叶子包。
	// 前值：bf4c78672ba265ed449f7385aac67c3918d004ffc67a5ba63744d41e5bb3931d（P5-3-A 之后，债 s1-a-class 之前）
	// 再前值：6b10a3e4fb641a39c03fcdc15224d78bad1c1f30ffd44eb0867831a17ddc8b0b（P5-1 之后）
	// 再前值：f78fff5868b1953208e62adda01f5ec87b929ac4fe68ec4abd10ae741f2d7db8（P5-1 之前）
	allowMatrixFingerprint = "d4164844746b06667580c3d2e2d4c8daeac0c25124994932cea6d8ca92f46da3"

	// allMax 限制 all: true 的规则条数，精确相等、只许改小。5 条分别是
	// cmd/server（唯一装配点）、tools/、demo/（消费者入口）、internal/proxy
	// （执行面）、internal/service（控制器层）——都是「装配点/执行面」这类真的
	// 需要看全局的位置。all: true 是最危险的放宽方向（一条规则等于整棵子树立即
	// 可依赖），单列上限是为了让「把 to 换成 all」除了指纹变化之外还有第二道可读信号。
	allMax = 5
)

// canonicalRule 把一条规则规范成一行无歧义的字符串：字段顺序固定，to/deny 先排序，
// from 用 %q 转义。因此「元素顺序不同」得到同一行，而「多/少/改任何一个元素」
// （包括把 to 清空并置 all）必然得到不同行。
func canonicalRule(r allowRule) string {
	to := append([]string(nil), r.to...)
	sort.Strings(to)
	deny := append([]string(nil), r.deny...)
	sort.Strings(deny)
	return fmt.Sprintf("from=%q all=%t to=[%s] deny=[%s]",
		r.from, r.all, strings.Join(to, " "), strings.Join(deny, " "))
}

// policyFingerprint 返回整张策略的规范化序列化 + sha256：
// 每条 allowRule 规范化成一行后整体排序（等价于按 from 排序，同 from 时按内容
// 排序，因此结果确定），leafRule 作为独立的 leaf 行追加，不会与 allowRules 混淆。
func policyFingerprint(pol policy) string {
	lines := make([]string, 0, len(pol.rules))
	for _, r := range pol.rules {
		lines = append(lines, "allow\t"+canonicalRule(r))
	}
	sort.Strings(lines)

	var b strings.Builder
	b.WriteString("import-graph-policy/v1\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("leaf\t" + canonicalRule(pol.fallback) + "\n")

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// countAllRules 数 all: true 的规则条数（fallback 不带 all，不计入）。
func countAllRules(pol policy) int {
	n := 0
	for _, r := range pol.rules {
		if r.all {
			n++
		}
	}
	return n
}

// TestImportGraphAllowMatrixFingerprint 是加宽 allow 矩阵的唯一机械信号：
// 绿表示矩阵与钉住的指纹一致；红表示矩阵被改动，必须先说明理由再更新常量。
func TestImportGraphAllowMatrixFingerprint(t *testing.T) {
	got := policyFingerprint(productionPolicy())
	require.Equalf(t, allowMatrixFingerprint, got,
		`allow 矩阵被改动：加宽任何 to/deny/all 都必须显式更新指纹，并在评审里说明理由——这是加宽的唯一机械信号。
实际指纹：%s
钉住指纹：%s
收紧（删规则 / 收窄 to / 多一个 deny）时把上面常量改成实际值即可；加宽时必须写清理由。
注意：没有任何机制能阻止有意同时改矩阵与指纹，指纹只保证这类改动不可静默，不是不可发生。`, got, allowMatrixFingerprint)
}

// checkAllCap 检查 all: true 的规则条数是否精确等于 max（只许改小）。
func checkAllCap(pol policy, max int) error {
	got := countAllRules(pol)
	if got != max {
		return fmt.Errorf("all: true 的规则有 %d 条，上限常量 allMax=%d：all 是最危险的放宽方向（一条规则等于整棵子树立即可依赖），只允许改小；新增必须单独提交，说明为什么不能用 to: 白名单", got, max)
	}
	return nil
}

// TestImportGraphAllRuleCap 给最危险的放宽方向（all: true）单列第二道信号。
func TestImportGraphAllRuleCap(t *testing.T) {
	require.NoError(t, checkAllCap(productionPolicy(), allMax))
}

// ---------------------------------------------------------------------------
// 债务账本（切片 + 构建期查重；条数必须精确等于上限）
// ---------------------------------------------------------------------------

type phase string

const (
	phaseP2   phase = "P2"
	phaseP3   phase = "P3"
	phaseP4   phase = "P4"
	phaseP6   phase = "P6"
	phaseKeep phase = "不消除"
)

// legacyRow 是账本的一行：一句理由 + 计划阶段。用切片而不是 map 字面量，
// 是为了在构建期查出重复 (from,to)——map 字面量重复键会静默 last-wins，条数失真。
type legacyRow struct {
	from   string
	to     string
	reason string
	phase  phase
}

// ledger 把「债务」与「结构性不消除」分开计上限：
// 债务条数必须精确等于 debtMax；结构性不消除条数必须精确等于 keepMax。
type ledger struct {
	name    string
	debt    []legacyRow
	debtMax int
	keep    []legacyRow
	keepMax int
}

type legacyEntry struct {
	reason string
	phase  phase
}

func (l ledger) build() (map[edge]legacyEntry, error) {
	out := make(map[edge]legacyEntry, len(l.debt)+len(l.keep))
	add := func(rows []legacyRow, keep bool) error {
		for _, r := range rows {
			e := edge{from: r.from, to: r.to}
			if _, dup := out[e]; dup {
				return fmt.Errorf("%s：重复的 legacy 边 %s（切片查重；不许用重复键掩盖计数）", l.name, e)
			}
			if strings.TrimSpace(r.reason) == "" {
				return fmt.Errorf("%s：legacy %s 缺少理由", l.name, e)
			}
			if keep && r.phase != phaseKeep {
				return fmt.Errorf("%s：legacy %s 在 keep 区，但阶段是 %q", l.name, e, r.phase)
			}
			if !keep && !r.phase.validNarrow() {
				return fmt.Errorf("%s：legacy %s 在债务区，阶段必须是 P2/P3/P4/P6，实际 %q", l.name, e, r.phase)
			}
			out[e] = legacyEntry{reason: r.reason, phase: r.phase}
		}
		return nil
	}
	if err := add(l.debt, false); err != nil {
		return nil, err
	}
	if err := add(l.keep, true); err != nil {
		return nil, err
	}
	return out, nil
}

func (l ledger) checkCaps() error {
	if len(l.debt) != l.debtMax {
		return fmt.Errorf("%s：债务 %d 条，上限常量 %d：只允许同步改小；新增必须单独提交并写理由",
			l.name, len(l.debt), l.debtMax)
	}
	if len(l.keep) != l.keepMax {
		return fmt.Errorf("%s：结构性不消除 %d 条，上限常量 %d：同样只允许改小",
			l.name, len(l.keep), l.keepMax)
	}
	return nil
}

// validNarrow 只认 P2/P3/P4/P6：phaseKeep 必须放进 keep 区，不能混进债务计数。
//
// P5 已收口（P5 的三条债都清完了）：这个档位不再存在，新增债**不许**再用
// "P5"——新增债走它自己的阶段。不重命名 P2/P3/P4/P6：改名会污染全部债行的
// 可读性，而且阶段号是历史指向，不是连续分组。债务条数上限 debtMax 不变（本来就没有 P5 行）。
func (p phase) validNarrow() bool {
	switch p {
	case phaseP2, phaseP3, phaseP4, phaseP6:
		return true
	}
	return false
}

var productionLedger = ledger{
	name:    "生产侧 Imports",
	debtMax: 16,
	keepMax: 2,
	debt: []legacyRow{
		{"internal/ent", "internal/domain", "Ent 生成模型复用 domain 枚举，Codegen 边界尚未抽离；P3 把枚举归入 api/中立类型并重生成，生成代码不得作为永久豁免", phaseP3},
		{"internal/ent/schema", "internal/domain", "手写 schema 字段直接用 domain 枚举；P3 归入中立类型后，schema 只依赖 ent 自带类型", phaseP3},
		{"internal/handler", "internal/repository", "handler 直读 repository（模板/订单/账号列表）；P4 存储接口化后改走 service 端口", phaseP4},
		{"internal/handler/paymentadmin", "internal/repository", "支付管理 handler 直读 repository；P4 后走 service 端口", phaseP4},
		{"internal/handler/user", "internal/repository", "用户端 handler 直读 repository（订单/钥匙/兑换）；P4 后走 service 端口", phaseP4},
		{"internal/quality", "internal/repository", "quality 的质量/流量落库直接用 repository；P4 存储接口化", phaseP4},
		{"internal/repository", "internal/billing", "repository 反向依赖 billing 的账务类型；P4 把账务读写抽成端口", phaseP4},
		{"internal/repository", "internal/supplier", "repository 反向依赖 supplier 聚合；P4 把供应商视图抽成端口", phaseP4},
		{"internal/rule", "internal/repository", "rule 引擎直接落库；P4 存储接口化", phaseP4},
		{"internal/sdkbridge", "internal/repository", "sdkbridge 直接写 repository（用量/状态）；P4 存储接口化", phaseP4},
		{"internal/handler", "internal/pricing", "handler 直接用 pricing 的报价/成本类型；P6 控制器化后走 service 端口", phaseP6},
		{"internal/handler", "internal/sdkbridge", "handler 的 codex 导入/用量接口直接调 sdkbridge；P6 收进 service", phaseP6},
		{"internal/handler", "internal/server", "只为 server.UserIDFromContext 一个 context 访问器而反向依赖传输壳；P1 零生产改动无法即消，P3 把该访问器移到 auth/api-meta 后本边消失", phaseP3},
		{"internal/handler/user", "internal/rule", "用户端 handler 直接用 rule 类型做设置转换；P6 控制器化", phaseP6},
		{"internal/server", "internal/handler/httpface", "server 用 httpface 的错误响应类型；P6 把 httpface 移出 handler 子包", phaseP6},
		{"internal/auth", "internal/handler/httpface", "接口包寄生：auth 中间件直接用 httpface 的错误体；P6 移出后消除", phaseP6},
	},
	keep: []legacyRow{
		{"internal/db/migrate/gen", "internal/repository", "迁移 DDL 生成器（go:generate）必须读取 repository 的 baseline DDL 与 hook 排除项；它是离线工具链、不是运行期包，结构上永不可能消除", phaseKeep},
		{"internal/server", "internal/auth", "server 是 chi 路由装配点，必须注入 auth.Issuer/SnapshotProvider 做中间件；装配点对认证实现的依赖是结构性的", phaseKeep},
	},
}

var testLedger = ledger{
	name:    "同包测试 TestImports",
	debtMax: 5,
	keepMax: 2,
	debt: []legacyRow{
		{"internal/billing", "internal/usage", "billing 测试用 usage 组件落库构造误差日志夹具；P4 后改 repository 端口", phaseP4},
		{"internal/verification", "internal/service", "verification 测试用 service.EmailCodeStore 做接口一致性断言；P4 把端口接口下沉", phaseP4},
		{"internal/handler", "internal/scheduler", "handler 测试直连 scheduler 构造路由视图；P6 后改 service 端口", phaseP6},
		{"internal/handler", "internal/settingssnap", "handler 测试读 settingssnap 构造设置快照；P6 后由 service 提供", phaseP6},
		{"internal/handler/payment", "internal/settingssnap", "支付回调测试用 settingssnap 构造设置；P6 后由 service 提供", phaseP6},
	},
	keep: []legacyRow{
		{"internal/server", "internal/config", "server 测试用 config 构造 service 依赖；测试装配点没有运行期对应物，结构上不会消失", phaseKeep},
		{"internal/proxy", "internal/service", "proxy 的 PG 集成测试用 service.New 构造 attempt-chain 查询夹具；服务层是查询语义的唯一入口，测试夹具结构性需要", phaseKeep},
	},
}

var externalTestLedger = ledger{
	name:    "外部测试包 XTestImports",
	debtMax: 0,
	keepMax: 3,
	keep: []legacyRow{
		{"internal/repository", "internal/proxy", "夹具跨层构造：repository_test 用 proxy 验证凭据/转发列的真实写入，外部测试夹具结构性需要", phaseKeep},
		{"internal/repository", "internal/scheduler", "夹具跨层构造：repository_test 用 scheduler 复现模板装载路径，外部测试夹具结构性需要", phaseKeep},
		{"internal/repository", "internal/usage", "夹具跨层构造：repository_test 用 usage 落库校验统计行，外部测试夹具结构性需要", phaseKeep},
	},
}

// ---------------------------------------------------------------------------
// 图扫描（go list，一次，缓存）
// ---------------------------------------------------------------------------

// scanRoots 是本门禁负责的顶层目录。新增顶层代码目录时必须在这里登记，
// 否则 TestImportGraphScanCoverage 会红（不允许静默漏包）。
var scanRoots = []string{"internal", "cmd", "pkg", "tools", "demo"}

// unscannedDirs 是「含 .go 文件但默认 go list 不列出」的目录，必须写明理由：
// 新增这类目录而不登记，TestImportGraphScanCoverage 会红。
var unscannedDirs = map[string]string{
	"tools/e2e": "全文件 //go:build e2e，默认构建不列出；由 e2e 流水线单独构建",
	"tmp":       "临时/调试目录，可能含互相冲突的 main 包；不参与分层，也不进 go list",
	"vendor":    "依赖副本（当前无），第三方代码不受本仓库分层约束",
	"testdata":  "测试夹具目录（Go 工具链本就忽略），里面不是可 import 的包",
}

type importGraph struct {
	pkgs  []string            // 扫描到的包（已去 modulePath 前缀），升序
	prod  map[string][]string // from -> Imports（原始 import path）
	test  map[string][]string // from -> TestImports（同包测试）
	xtest map[string][]string // from -> XTestImports（外部测试包 xxx_test）
}

var (
	scanOnce    sync.Once
	scanGraph   *importGraph
	scanElapsed time.Duration
	scanErr     error
)

func cachedScan() (*importGraph, time.Duration, error) {
	scanOnce.Do(func() {
		scanGraph, scanElapsed, scanErr = runGoList()
	})
	return scanGraph, scanElapsed, scanErr
}

func runGoList() (*importGraph, time.Duration, error) {
	root, err := repositoryRoot()
	if err != nil {
		return nil, 0, err
	}
	patterns, err := scanPatterns(root)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()

	args := []string{
		"list",
		"-f", `{{.ImportPath}}|{{join .Imports " "}}|{{join .TestImports " "}}|{{join .XTestImports " "}}`,
	}
	args = append(args, patterns...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	// 干净环境：外部 GOFLAGS（例如 -tags=integration）会改变包集，让门禁少扫而不自知。
	cmd.Env = cleanEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	out, err := cmd.Output()
	elapsed := time.Since(start)
	if ctx.Err() == context.DeadlineExceeded {
		return nil, elapsed, fmt.Errorf("go list 超过 %s 超时兜底（构建缓存过冷？）", scanTimeout)
	}
	if err != nil {
		return nil, elapsed, fmt.Errorf("go list 失败：%w；stderr：%s", err, strings.TrimSpace(stderr.String()))
	}
	g, err := parseGoList(out)
	if err != nil {
		return nil, elapsed, err
	}
	return g, elapsed, nil
}

// cleanEnv 去掉调用方可能设置的 GOFLAGS / GO111MODULE，再显式固定。
func cleanEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GOFLAGS=") || strings.HasPrefix(kv, "GO111MODULE=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GOFLAGS=", "GO111MODULE=on")
}

// scanPatterns 只为真实存在的顶层目录生成 ./x/... pattern：demo/ 这类目录在某些
// checkout 上不存在，直接把 ./demo/... 交给 go list 会整体报错。
func scanPatterns(root string) ([]string, error) {
	var patterns []string
	for _, dir := range scanRoots {
		info, err := os.Stat(filepath.Join(root, dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			continue
		}
		patterns = append(patterns, "./"+dir+"/...")
	}
	if len(patterns) == 0 {
		return nil, errors.New("没有任何可扫描的顶层目录，扫描口径失效")
	}
	return patterns, nil
}

func repositoryRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("无法定位本测试源码路径，无法推断仓库根")
	}
	// internal/architecture/importgraph_test.go -> 仓库根
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

func parseGoList(out []byte) (*importGraph, error) {
	g := &importGraph{
		prod:  map[string][]string{},
		test:  map[string][]string{},
		xtest: map[string][]string{},
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("无法解析 go list 输出行：%q", line)
		}
		pkg := strings.TrimPrefix(parts[0], modulePath)
		if pkg == parts[0] { // 不是本模块的包，理论上不会出现
			continue
		}
		if matchesPrefix(pkg, "vendor") || matchesPrefix(pkg, "tmp") {
			continue
		}
		g.pkgs = append(g.pkgs, pkg)
		g.prod[pkg] = strings.Fields(parts[1])
		g.test[pkg] = strings.Fields(parts[2])
		g.xtest[pkg] = strings.Fields(parts[3])
	}
	if len(g.pkgs) == 0 {
		return nil, errors.New("go list 返回空白图：扫描口径失效（不允许 skip，必须红）")
	}
	sort.Strings(g.pkgs)
	return g, nil
}

// internalEdges 只保留两端都在本模块内的边，并把 to 去掉 modulePath 前缀。
func internalEdges(imports map[string][]string) []edge {
	out := make([]edge, 0, len(imports))
	for from, tos := range imports {
		for _, to := range tos {
			if !strings.HasPrefix(to, modulePath) {
				continue
			}
			out = append(out, edge{from: from, to: strings.TrimPrefix(to, modulePath)})
		}
	}
	sortEdges(out)
	return out
}

func subtractEdges(a, b []edge) []edge {
	drop := make(map[edge]bool, len(b))
	for _, e := range b {
		drop[e] = true
	}
	out := make([]edge, 0, len(a))
	for _, e := range a {
		if !drop[e] {
			out = append(out, e)
		}
	}
	sortEdges(out)
	return out
}

func sortEdges(edges []edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		return edges[i].to < edges[j].to
	})
}

// opaqueDirNames 是「即使含 .go 文件也不是可 import 的包」的目录名，按 basename
// 在任意深度生效，与 go list 的语义一致：
//   - testdata：Go 工具链显式忽略，里面只会是夹具；
//   - vendor：依赖副本，第三方代码不受本仓库分层约束；
//   - tmp：临时/调试目录，可能含互相冲突的 main 包；
//   - node_modules：不装 Go 包。
//
// 只有根级的 testdata / tmp / vendor 需要另行在 unscannedDirs 里写明理由（它们
// 解释「为什么根级这些目录不被扫」），任意深度的同名目录既不深入也不登记。
var opaqueDirNames = map[string]string{
	"testdata":     "Go 工具链显式忽略，里面只会是夹具",
	"tmp":          "临时/调试目录，可能含互相冲突的 main 包",
	"vendor":       "依赖副本，第三方代码不受本仓库分层约束",
	"node_modules": "不装 Go 包",
}

// dirsWithGoFiles 返回仓库里所有含 .go 文件的目录（相对路径，斜杠分隔）。
// 隐藏目录直接跳过（不可能装 Go 包）；opaqueDirNames 里的 basename 在任意深度都
// 不深入（见该变量注释），因此嵌套的 testdata 夹具不会变成假失败；根级的
// testdata / tmp / vendor 仍然登记到结果里，好让 unscannedDirs 的存在性检查
// （「声明了就必须被遍历看到」）对它们继续生效。
func dirsWithGoFiles(root string) ([]string, error) {
	var dirs []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		hasGo := false
		for _, e := range entries {
			if e.IsDir() {
				name := e.Name()
				if strings.HasPrefix(name, ".") {
					continue
				}
				child := filepath.Join(dir, name)
				if _, opaque := opaqueDirNames[name]; opaque {
					// 不深入。根级仍要登记，供 unscannedDirs 的存在性检查使用；
					// node_modules 不在 unscannedDirs 里，根级也不登记。
					if dir == root && name != "node_modules" {
						rel, err := filepath.Rel(root, child)
						if err != nil {
							return err
						}
						dirs = append(dirs, filepath.ToSlash(rel))
					}
					continue
				}
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			if strings.HasSuffix(e.Name(), ".go") {
				hasGo = true
			}
		}
		if !hasGo || dir == root {
			return nil
		}
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		dirs = append(dirs, filepath.ToSlash(rel))
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 校验器（纯函数：自测用合成图驱动，不碰真实仓库）
// ---------------------------------------------------------------------------

type findingKind string

const (
	kindNewViolation    findingKind = "new violation"
	kindHardViolation   findingKind = "hard rule violation"
	kindStaleLegacy     findingKind = "stale legacy"
	kindRedundantLegacy findingKind = "redundant legacy"
)

type finding struct {
	kind   findingKind
	edge   edge
	detail string
}

func (f finding) String() string {
	if f.edge.from == "" && f.edge.to == "" {
		return fmt.Sprintf("%s: %s", f.kind, f.detail)
	}
	return fmt.Sprintf("%s: %s（%s）", f.kind, f.edge, f.detail)
}

func checkEdges(edges []edge, pol policy, exempt map[edge]legacyEntry) []finding {
	present := make(map[edge]bool, len(edges))
	var out []finding

	sorted := append([]edge(nil), edges...)
	sortEdges(sorted)
	for _, e := range sorted {
		present[e] = true
		j := pol.judge(e)
		if j.allowed {
			continue
		}
		switch {
		case isHardRuleEdge(e):
			out = append(out, finding{
				kind:   kindHardViolation,
				edge:   e,
				detail: "pkg/* 不得 import internal/*，该规则没有 legacy 通道",
			})
		default:
			if _, exempted := exempt[e]; exempted {
				continue // 既有债务：由 stale 检查保证它只能变短。
			}
			out = append(out, finding{
				kind:   kindNewViolation,
				edge:   e,
				detail: j.rule + "；若属既有债务：单独提交、登记 legacy{reason, phase} 并同步上限常量",
			})
		}
	}

	keys := make([]edge, 0, len(exempt))
	for e := range exempt {
		keys = append(keys, e)
	}
	sortEdges(keys)
	for _, e := range keys {
		en := exempt[e]
		switch {
		case !present[e]:
			out = append(out, finding{
				kind:   kindStaleLegacy,
				edge:   e,
				detail: fmt.Sprintf("图中已无此边，删除该条并把上限减 1（原计划 %s）", en.phase),
			})
		case pol.judge(e).allowed:
			out = append(out, finding{
				kind:   kindRedundantLegacy,
				edge:   e,
				detail: fmt.Sprintf("该边已符合目标方向，删除该条并把上限减 1（原计划 %s）", en.phase),
			})
		}
	}
	return out
}

// checkPrefixHygiene 检查每条 to/deny 前缀：不许裸 internal / internal/（一次放行全树），
// 且必须能在扫描到的包集合里命中自身或子树（防 internal/handlerevil 这类拼写白名单）。
func checkPrefixHygiene(pol policy, pkgs []string) []string {
	var problems []string
	check := func(owner, prefix string) {
		if prefix == "" {
			problems = append(problems, owner+" 有空前缀（等于全放行）")
			return
		}
		if prefix == "internal" || prefix == "internal/" {
			problems = append(problems, fmt.Sprintf("%s 使用裸前缀 %q：等于一次放行整棵 internal 树", owner, prefix))
			return
		}
		for _, p := range pkgs {
			if matchesPrefix(p, prefix) {
				return
			}
		}
		problems = append(problems, fmt.Sprintf("%s 的前缀 %q 在扫描结果里没有任何包能命中（拼写错误？）", owner, prefix))
	}
	for _, r := range pol.rules {
		owner := fmt.Sprintf("规则 %q", r.from)
		for _, p := range r.to {
			check(owner, p)
		}
		for _, p := range r.deny {
			check(owner, p)
		}
	}
	for _, p := range pol.fallback.to {
		check("fallback", p)
	}
	return problems
}

// ---------------------------------------------------------------------------
// 真实仓库的门禁
// ---------------------------------------------------------------------------

// TestImportGraphProduction 是主门禁：生产侧 import 必须落在 allow ∪ 账本，
// 且账本只能变短。
func TestImportGraphProduction(t *testing.T) {
	g := mustScan(t)
	exempt := useLedger(t, productionLedger)
	for e := range exempt {
		require.Falsef(t, isHardRuleEdge(e), "pkg/* -> internal/* 不允许登记 legacy：%s", e)
	}

	edges := internalEdges(g.prod)
	findings := checkEdges(edges, productionPolicy(), exempt)
	assertNoFindings(t, "生产侧", findings)
	t.Logf("生产侧：%d 个包、%d 条模块内 import 边", len(g.pkgs), len(edges))
}

// TestImportGraphTestWidening 裁决「只有同包测试才有」的边：口径更宽，但同样 shrink-only。
func TestImportGraphTestWidening(t *testing.T) {
	g := mustScan(t)
	exempt := useLedger(t, testLedger)

	testOnly := subtractEdges(internalEdges(g.test), internalEdges(g.prod))
	require.NotEmpty(t, testOnly, "TestImports 为空说明扫描口径坏了")

	findings := checkEdges(testOnly, productionPolicy().widened(testFixtureAllow), exempt)
	assertNoFindings(t, "同包测试（TestImports 宽口径）", findings)
	t.Logf("同包测试：仅测试才有的边 %d 条", len(testOnly))
}

// TestImportGraphExternalTestWidening 裁决外部测试包（package xxx_test，XTestImports）
// 独有的边：这类包不在生产 import 图里，必须单独遮罩，否则会静默漏包。
func TestImportGraphExternalTestWidening(t *testing.T) {
	g := mustScan(t)
	exempt := useLedger(t, externalTestLedger)

	prod := internalEdges(g.prod)
	xtestOnly := subtractEdges(internalEdges(g.xtest), prod)
	xtestOnly = subtractEdges(xtestOnly, internalEdges(g.test))
	require.NotEmpty(t, xtestOnly, "XTestImports 为空说明扫描口径坏了")

	findings := checkEdges(xtestOnly, productionPolicy().widened(testFixtureAllow), exempt)
	assertNoFindings(t, "外部测试包（XTestImports 宽口径）", findings)
	t.Logf("外部测试包：仅外部测试才有的边 %d 条", len(xtestOnly))
}

// TestImportGraphScanCoverage 双向断言包集：
// pkgs == dirs - unscannedDirs。少扫要红，多扫同样要红（多扫也要写理由）。
func TestImportGraphScanCoverage(t *testing.T) {
	g := mustScan(t)
	root, err := repositoryRoot()
	require.NoError(t, err)

	dirs, err := dirsWithGoFiles(root)
	require.NoError(t, err)
	require.NotEmpty(t, dirs)

	dirSet := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		dirSet[d] = true
	}
	pkgSet := make(map[string]bool, len(g.pkgs))
	for _, p := range g.pkgs {
		pkgSet[p] = true
	}

	var undeclared, extra, stale []string
	for _, d := range dirs {
		if pkgSet[d] {
			if _, ok := unscannedDirs[d]; ok {
				stale = append(stale, d) // 声明为不扫，但实际被扫到了
			}
			continue
		}
		if _, ok := unscannedDirs[d]; !ok {
			undeclared = append(undeclared, d) // 有 .go 文件却没被扫到，也没声明
		}
	}
	for _, p := range g.pkgs {
		if !dirSet[p] {
			extra = append(extra, p) // 扫到了文件系统遍历之外的包，同样要说明
		}
	}

	require.Emptyf(t, undeclared, "以下目录含 .go 文件但没被 go list 扫到，也没在 unscannedDirs 里声明：%v", undeclared)
	require.Emptyf(t, extra, "go list 扫到了文件系统遍历没看到的包（多扫也要显式说明）：%v", extra)
	require.Emptyf(t, stale, "以下目录已能被 go list 扫到，请从 unscannedDirs 删掉：%v", stale)

	for d, reason := range unscannedDirs {
		require.NotEmptyf(t, strings.TrimSpace(reason), "unscannedDirs[%s] 缺少理由", d)
		// tmp/、vendor/ 是 gitignored，允许为它们预先声明（目录不存在时不报错）；
		// 目录存在时必须被目录遍历看到，避免拼写错误混进来。
		if info, err := os.Stat(filepath.Join(root, d)); err == nil && info.IsDir() {
			require.Truef(t, containsString(dirs, d), "unscannedDirs 里的 %q 存在但目录遍历没看到它", d)
		}
	}
}

// TestImportGraphSingleCompositionRoot 钉住「cmd/server 是唯一装配点」：
// cmd/ 下不允许悄然多出第二个 main。
func TestImportGraphSingleCompositionRoot(t *testing.T) {
	g := mustScan(t)
	var roots []string
	for _, pkg := range g.pkgs {
		if strings.HasPrefix(pkg, "cmd/") {
			roots = append(roots, pkg)
		}
	}
	require.Equal(t, []string{"cmd/server"}, roots, "cmd/ 下只允许一个装配点；新增 main 必须显式评审")
}

// TestImportGraphPolicyCoversPackages 保证没有包偷偷落到兜底叶子规则：
// 新包必须显式声明层级（pkg/* 由硬规则覆盖）。
func TestImportGraphPolicyCoversPackages(t *testing.T) {
	g := mustScan(t)
	pol := productionPolicy()

	var missing []string
	for _, pkg := range g.pkgs {
		if strings.HasPrefix(pkg, "pkg/") {
			continue
		}
		if _, ok := pol.ruleFor(pkg); !ok {
			missing = append(missing, pkg)
		}
	}
	require.Emptyf(t, missing, "以下包没有显式层级声明，请在 allowRules 里补一条：%v", missing)
}

// TestImportGraphPrefixHygiene 防「白名单写歪」：裸 internal/ 与拼错的前缀都要红。
func TestImportGraphPrefixHygiene(t *testing.T) {
	g := mustScan(t)

	problems := checkPrefixHygiene(productionPolicy(), g.pkgs)
	require.Emptyf(t, problems, "allow 矩阵前缀不合法：%v", problems)

	fixtures := policy{fallback: allowRule{from: "<test fixtures>", to: testFixtureAllow}}
	require.Emptyf(t, checkPrefixHygiene(fixtures, g.pkgs), "测试夹具前缀不合法")
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// useLedger 构建账本并断言条数精确等于上限；失败信息指向该改哪个常量。
func useLedger(t *testing.T, l ledger) map[edge]legacyEntry {
	t.Helper()
	exempt, err := l.build()
	require.NoErrorf(t, err, "%s 的账本构建失败", l.name)
	require.NoErrorf(t, l.checkCaps(), "%s 的条数上限未对齐", l.name)
	logLedger(t, l)
	return exempt
}

func mustScan(t *testing.T) *importGraph {
	t.Helper()
	g, elapsed, err := cachedScan()
	require.NoError(t, err, "import-graph 扫描失败")
	require.NotNil(t, g)
	require.GreaterOrEqual(t, len(g.pkgs), minPackages, "扫描到的包数异常偏少，门禁可能退化成空图")
	if elapsed > scanWarnBudget {
		t.Logf("WARN：go list 扫描耗时 %s，超过 %s 目标；本单元必须保持纯静态、可缓存", elapsed.Round(time.Millisecond), scanWarnBudget)
	}
	t.Logf("go list 扫描：%d 个包，耗时 %s（目标 < %s，只 log 不断言）", len(g.pkgs), elapsed.Round(time.Millisecond), scanWarnBudget)
	return g
}

func assertNoFindings(t *testing.T, scope string, findings []finding) {
	t.Helper()
	if len(findings) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s：发现 %d 处分层问题（账本只允许变短）：", scope, len(findings))
	for _, f := range findings {
		b.WriteString("\n  - ")
		b.WriteString(f.String())
	}
	t.Error(b.String())
}

func logLedger(t *testing.T, l ledger) {
	t.Helper()
	t.Logf("%s：债务 %d/%d 条，结构性不消除 %d/%d 条", l.name, len(l.debt), l.debtMax, len(l.keep), l.keepMax)
	for _, r := range append(append([]legacyRow(nil), l.debt...), l.keep...) {
		t.Logf("  %s -> %s | %s | %s", r.from, r.to, r.reason, r.phase)
	}
}
