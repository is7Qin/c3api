// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 代际符号的机械枚举门禁。四个代际（C 客户端 CAS 令牌 / K 身份纪元 / I 候选指纹 /
// G 组发布代际）在源码里以**独立命名**出现，只搜 `lifecycle_revision` 与
// `LifecycleRevision` 两个拼写覆盖不到清单：`ExpectedRevision`、`MaxLifecycleRevision`、
// `AccountRevision`、三个键结构里的第三分量、`failureRetryTask.revision` 都是各自
// 独立的名字。本文件按 `go/ast` 枚举，用**显式声明的常量**承载允许集与词表——新增
// 一处 C 读取、或新起一个 revision 命名的标识符，都必须有意改这些常量。
//
// 为什么不用文本搜索：文本搜索无法区分"读到账号的 C 字段"（判据）与"标识符名字里
// 含 LifecycleRevision"（如哨兵名 `ErrMissingExpectedRevision`），也无法区分字段
// 声明与字段读取。AST 能把两者分开，故断言落在**选择器读取**上。

// scanDirs 是符号枚举面：承载代际语义（C / K / 失效 / 健康 / continuation / 尝试
// 身份 / 管理面写面）的全部手写包。生成代码（`internal/ent`、`*.gen.go`）不在内——
// 其符号形态由 ent 与 openapi 生成器持有，改它没有意义；文档与 web 也不在
// AST 面内（它们由文档评审覆盖）。
var scanDirs = []string{
	"internal/scheduler",
	"internal/latch",
	"internal/continuation",
	"internal/sdkbridge",
	"internal/proxy",
	"internal/rule",
	"internal/domain",
	"internal/repository",
	"internal/service",
	"internal/handler",
	"cmd/server",
}

// inFlightDirs 是在途工件判定的归属包：失效持久化 / latch / 健康门槛 /
// continuation 绑定 / 尝试身份。这些包里的代码决定"在途工件是否仍然有效"，
// 故一律按 (I,K) 判定，不得以 C 作判据。
var inFlightDirs = []string{
	"internal/scheduler",
	"internal/latch",
	"internal/continuation",
	"internal/sdkbridge",
	"internal/proxy",
	"cmd/server",
}

// cFieldSpellings 是 C 在 Go 源码里**作为字段/选择器**出现的全部拼写。
// 哨兵名（`ErrStaleRevision` 等）与类型名（`AccountRevision`）不在此列：它们不是
// 字段读取，出现在在途包里不构成"以 C 作判据"。
var cFieldSpellings = []string{
	"LifecycleRevision",    // 账号行与域模型的 C
	"ExpectedRevision",     // recover body 的客户端前置条件字段
	"MaxLifecycleRevision", // 编译水位（DB 变更检测）
}

// cReadsAllowedInFlight 是在途包内**唯一**允许出现 C 字段读取的站点。编译水位不是
// fence：它是"DB 有没有变"的检测量，C 无条件 +1 正是它成立的前提。声明为常量，
// 故新增或移除该读取都必须显式改这里。
var cReadsAllowedInFlight = []string{
	"internal/scheduler/compile_backstop.go#snapshotToProbeCounts",
}

// cReadWhitelist 是"允许以 C 作客户端前置条件 / 响应回显 / 重编译水位"的站点全集
// （`dir/file#func`），值是该站点保留 C 的理由。断言是**相等**（双向）：新增一处
// C 读取 ⇒ 失败；把某处 C 读取修掉 ⇒ 该条目变陈旧 ⇒ 同样失败，强制显式收缩。
var cReadWhitelist = map[string]string{
	"internal/handler/account.go#PostAccountsBatchUpdate":          "回显：批量更新响应逐账号携带新 C，供客户端下次 CAS",
	"internal/handler/account.go#PostAccountsIdRecover":            "前置条件：recover body 的 expected_revision（对 C）",
	"internal/handler/convert.go#toAPIAccount":                     "回显：账号响应携带 C",
	"internal/handler/convert.go#toAPIAccountView":                 "回显：账号视图携带 C",
	"internal/repository/account_repo.go#CreateAccount":            "写入：创建时把入参 C 投影到行",
	"internal/repository/batch.go#UpdateAccountsBatch":             "回显：批量写把行锁内重取的新 C 回传",
	"internal/repository/group_repo.go#CompileStalenessSnapshot":   "水位：MAX(lifecycle_revision) 是重编译触发量",
	"internal/repository/mapping.go#toDomainAccount":               "回显：行 → 域映射携带 C",
	"internal/scheduler/compile_backstop.go#snapshotToProbeCounts": "水位：编译兜底探针的 C 上界",
	"internal/service/account.go#PatchAccount":                     "前置条件：If-Match 对 C（陈旧 → 412）",
	"internal/service/ext_codex.go#UpsertAccountExt":               "前置条件：凭据写乐观重试环以 C 作活性守卫（幂等 PUT 不推进 K，故守卫不能用 K）",
	"internal/service/ext_codex_import.go#updateCodexCredentials":  "前置条件：导入写对 C",
}

// structFieldSpec 钉住承载身份/内容代际的结构体字段名：更名后的拼写必须在位，
// 被替换掉的拼写必须缺席。字段名就是判据的可读契约——`Revision` 这类泛化名会让
// 调用点把 C 静默传进来，故键结构一律显式带 Identity。
type structFieldSpec struct {
	dir        string
	typ        string
	wantFields []string
	noFields   []string
	wantTags   []string
	noTags     []string
}

var structFieldSpecs = []structFieldSpec{
	{"internal/latch", "LatchKey",
		[]string{"AccountID", "Fingerprint", "IdentityRevision"}, []string{"Revision"}, nil, nil},
	{"internal/latch", "latchKey",
		[]string{"AccountID", "Fingerprint", "IdentityRevision"}, []string{"Revision"}, nil, nil},
	{"internal/continuation", "Binding",
		[]string{"AccountID", "Fingerprint", "IdentityRevision"}, []string{"Revision"}, nil, nil},
	{"internal/continuation", "redisWire",
		[]string{"AccountID", "Fingerprint", "IdentityRevision"}, []string{"Revision"},
		[]string{`json:"identity_revision"`}, []string{`json:"revision"`}},
	{"internal/scheduler", "HealthKey",
		[]string{"AccountID", "Quality", "Identity", "IdentityRevision"}, []string{"Revision"}, nil, nil},
	{"internal/scheduler", "Attempt",
		[]string{"AccountID", "CandidateFingerprint", "IdentityRevision"}, []string{"LifecycleRevision"}, nil, nil},
	{"internal/rule", "Event",
		[]string{"AccountID", "CandidateFingerprint", "ExpectedIdentityRevision"},
		[]string{"ExpectedRevision", "LifecycleRevision"}, nil, nil},
	{"internal/sdkbridge", "failureRetryTask",
		[]string{"accountID", "fingerprint", "identityRevision"}, []string{"revision"}, nil, nil},
	// domain.Account 必须**同时**保留 C 与 K：C 是回显与客户端前置条件的载体，
	// K 是在途判据的载体。两者职责分离，不合并。
	{"internal/domain", "Account",
		[]string{"LifecycleRevision", "IdentityRevision"}, nil, nil, nil},
}

// revisionVocabulary 是本门禁的搜索词表：扫描面上出现的**全部**含 "revision"
// （不区分大小写）的标识符与结构体标签。断言是**相等**（双向）：新起一个 revision
// 命名的标识符就必须在此显式声明并写明它指哪个代际。
//
// 名字是可**重载**的：`expectedRevision` 在管理面前置条件里指 C，在运行时失效动词
// 里指 K。故此处只声明"该名字存在且已被审阅"，"哪里读 C"由 cReadWhitelist 与
// structFieldSpecs 按站点承担。
var revisionVocabulary = map[string]string{
	"AccountRevision":            "C：批量更新响应条目（回显）",
	"AddIdentityRevision":        "K：ent 相对自增",
	"AddLifecycleRevision":       "C：ent 相对自增（水位必须无条件推进）",
	"ErrMissingExpectedRevision": "名字含 C、语义是 K：失效事件的 K 缺失校验",
	"ErrProbeStaleRevision":      "哨兵：探针与当前身份不符 ⇒ fail-closed（比的是 I 与 K）",
	"ErrStaleFailureRevision":    "哨兵（sdkbridge 自持）：失效写陈旧",
	"ErrStaleIdentityRevision":   "K：失效路径 CAS 陈旧（与 C 前置条件陈旧分离）",
	"ErrStaleRevision":           "C：客户端前置条件陈旧",
	"ExpectedIdentityRevision":   "K：规则事件携带的身份纪元",
	"ExpectedRevision":           "C：recover body 的客户端前置条件字段",
	"IdentityRevision":           "K：身份纪元（域模型、键结构、尝试身份、观测面）",
	"IdentityRevisionEQ":         "K：ent 相等谓词",
	"LifecycleRevision":          "C：客户端 CAS 令牌（域模型、回显、前置条件）",
	"LifecycleRevisionEQ":        "C：ent 相等谓词（前置条件 WHERE）",
	"MaxLifecycleRevision":       "C：编译水位（DB 变更检测，非 fence）",
	"Revision":                   "泛化名：healthEntry 的 Redis 记录槽位（值 = K）",
	"SetIdentityRevision":        "K：ent 写入",
	"SetLifecycleRevision":       "C：ent 写入",
	"expectedIdentityRevision":   "K：参数名（失效路径）",
	"expectedRevision":           "可重载：管理面前置条件指 C，运行时失效动词指 K",
	"identityRevision":           "K：参数/字段名（latch、continuation、失效重试）",
	"revision":                   "泛化名：编译器事实的候选内容代际（值 = K）、SetProbing 的 K 参数",
}

// revisionTags 是扫描面上全部含 "revision" 的结构体标签（已去引号）。
var revisionTags = map[string]string{
	`json:"identity_revision"`: "K：continuation 绑定线格式的第三分量",
}

// --- 扫描基础设施 ---

// gateFile 是一个已解析的源文件。
type gateFile struct {
	dir  string // 相对模块根的斜杠路径
	name string
	fset *token.FileSet
	file *ast.File
}

// gateRoot 定位模块根：本文件在 <root>/internal/scheduler/ 下，向上找到含 go.mod 的目录。
func gateRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller 必须可用")
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "向上未找到 go.mod：模块根定位失败")
		dir = parent
	}
}

// gateSources 解析 dirs 下全部非测试、非生成代码的 .go 文件。
func gateSources(t *testing.T, root string, dirs []string) []gateFile {
	t.Helper()
	var out []gateFile
	for _, d := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(d))
		ents, err := os.ReadDir(abs)
		require.NoError(t, err, "扫描目录必须存在：%s", d)
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || strings.HasSuffix(n, ".gen.go") {
				continue
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, filepath.Join(abs, n), nil, parser.SkipObjectResolution)
			require.NoError(t, perr, "解析失败：%s/%s", d, n)
			out = append(out, gateFile{dir: d, name: n, fset: fset, file: f})
		}
	}
	require.NotEmpty(t, out, "扫描面不得为空")
	return out
}

// cRead 是一处 C 字段拼写的**选择器读取**及其所属顶层函数。比较必然蕴含读取，
// 故"无读取"比"无比较"更强。
type cRead struct {
	dir, file, fn, sel string
	line               int
}

func (r cRead) site() string { return r.dir + "/" + r.file + "#" + r.fn }

// gateCReads 枚举 srcs 中所有 C 字段拼写的选择器读取。
func gateCReads(t *testing.T, srcs []gateFile, spellings []string) []cRead {
	t.Helper()
	want := make(map[string]bool, len(spellings))
	for _, s := range spellings {
		want[s] = true
	}
	var out []cRead
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				se, ok := n.(*ast.SelectorExpr)
				if !ok || !want[se.Sel.Name] {
					return true
				}
				out = append(out, cRead{src.dir, src.name, fn.Name.Name, se.Sel.Name, src.fset.Position(se.Pos()).Line})
				return true
			})
		}
	}
	return out
}

func sortedUniqueSites(reads []cRead) []string {
	seen := make(map[string]bool, len(reads))
	out := make([]string, 0, len(reads))
	for _, r := range reads {
		if seen[r.site()] {
			continue
		}
		seen[r.site()] = true
		out = append(out, r.site())
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// --- 断言 ---

// TestInFlightPathsNeverReadTheClientToken 在途包内不存在以 C 作判据的读取：
// 允许出现的 C 字段读取站点必须**恰好**等于 cReadsAllowedInFlight（编译水位）。
// 相等断言双向生效——水位读取若消失（比如水位机制被改掉）同样失败。
func TestInFlightPathsNeverReadTheClientToken(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, inFlightDirs)
	got := sortedUniqueSites(gateCReads(t, srcs, cFieldSpellings))
	require.Equal(t, sortedStrings(cReadsAllowedInFlight), got,
		"在途包的工件判据必须按 (I,K)：C 只允许留在编译水位")
}

// TestClientTokenReadsStayInsideTheDeclaredWhitelist 全枚举面上每一处 C 读取都
// 必须落在显式声明的白名单站点内（前置条件 / 回显 / 水位）。白名单条目必须**活着**
// ——没有对应读取的条目意味着白名单在腐烂，也失败。
func TestClientTokenReadsStayInsideTheDeclaredWhitelist(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, scanDirs)
	got := sortedUniqueSites(gateCReads(t, srcs, cFieldSpellings))
	require.Equal(t, sortedKeys(cReadWhitelist), got,
		"C 读取集合与声明白名单必须相等：新增即失败，修掉即陈旧")
	for site, why := range cReadWhitelist {
		require.NotEmpty(t, strings.TrimSpace(why), "白名单条目必须写明保留 C 的理由：%s", site)
	}
	// 在途包必须被全枚举面覆盖，否则"在途无 C 读取"是空断言。
	for _, d := range inFlightDirs {
		require.Contains(t, scanDirs, d, "在途包必须属于枚举面：%s", d)
	}
}

// structShape 是一个结构体的字段名与标签集合。
type structShape struct {
	fields map[string]bool
	tags   map[string]bool
}

func gateStructShape(t *testing.T, srcs []gateFile, dir, typ string) (structShape, bool) {
	t.Helper()
	for _, src := range srcs {
		if src.dir != dir {
			continue
		}
		for _, decl := range src.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != typ {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				shape := structShape{fields: map[string]bool{}, tags: map[string]bool{}}
				for _, f := range st.Fields.List {
					if f.Tag != nil {
						if s, uerr := strconv.Unquote(f.Tag.Value); uerr == nil {
							shape.tags[s] = true
						}
					}
					for _, n := range f.Names {
						shape.fields[n.Name] = true
					}
				}
				return shape, true
			}
		}
	}
	return structShape{}, false
}

// TestIdentityCarryingStructsUseTheRenamedSpellings 承载身份/内容代际的结构体必须
// 用更名后的拼写，被替换掉的拼写必须缺席。这是"符号枚举"对**键结构**那一半的落点：
// `HealthKey.Revision`、`LatchKey.Revision`、`Binding.Revision`、
// `Attempt.LifecycleRevision`、`Event.ExpectedRevision`、`failureRetryTask.revision`
// 都不得复活。
func TestIdentityCarryingStructsUseTheRenamedSpellings(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, scanDirs)
	require.NotEmpty(t, structFieldSpecs, "字段表不得为空")
	for _, spec := range structFieldSpecs {
		shape, found := gateStructShape(t, srcs, spec.dir, spec.typ)
		require.True(t, found, "结构体必须存在：%s.%s", spec.dir, spec.typ)
		for _, f := range spec.wantFields {
			require.True(t, shape.fields[f], "%s.%s 必须带字段 %s（代际归属显式化）", spec.dir, spec.typ, f)
		}
		for _, f := range spec.noFields {
			require.False(t, shape.fields[f], "%s.%s 不得再有泛化/旧拼写字段 %s", spec.dir, spec.typ, f)
		}
		for _, tg := range spec.wantTags {
			require.True(t, shape.tags[tg], "%s.%s 必须带标签 %s", spec.dir, spec.typ, tg)
		}
		for _, tg := range spec.noTags {
			require.False(t, shape.tags[tg], "%s.%s 不得再有旧标签 %s", spec.dir, spec.typ, tg)
		}
	}
}

// TestRevisionVocabularyIsFullyDeclared 枚举面上每个含 "revision" 的标识符与结构体
// 标签都必须在词表里显式声明，且词表里每条都必须真的出现。前者拦住"新起一个 C
// 拼写而无人注意"，后者拦住"改名后词表腐烂"。
func TestRevisionVocabularyIsFullyDeclared(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, scanDirs)
	idents := map[string]bool{}
	tags := map[string]bool{}
	for _, src := range srcs {
		ast.Inspect(src.file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				if strings.Contains(strings.ToLower(v.Name), "revision") {
					idents[v.Name] = true
				}
			case *ast.Field:
				if v.Tag == nil {
					return true
				}
				if s, uerr := strconv.Unquote(v.Tag.Value); uerr == nil && strings.Contains(strings.ToLower(s), "revision") {
					tags[s] = true
				}
			}
			return true
		})
	}
	observedIdents := make([]string, 0, len(idents))
	for n := range idents {
		observedIdents = append(observedIdents, n)
	}
	sort.Strings(observedIdents)
	observedTags := make([]string, 0, len(tags))
	for n := range tags {
		observedTags = append(observedTags, n)
	}
	sort.Strings(observedTags)

	require.Equal(t, sortedKeys(revisionVocabulary), observedIdents,
		"枚举面上出现未声明的 revision 命名标识符")
	require.Equal(t, sortedKeys(revisionTags), observedTags,
		"枚举面上出现未声明的 revision 结构体标签")
	for name, why := range revisionVocabulary {
		require.NotEmpty(t, strings.TrimSpace(why), "词表条目必须写明该名字指哪个代际：%s", name)
	}
	// 词表必须覆盖 C 的全部字段拼写，否则"在途无 C 读取"可能只是没搜到。
	for _, s := range cFieldSpellings {
		require.Contains(t, revisionVocabulary, s, "C 字段拼写必须进词表：%s", s)
	}
}

// --- continuation：陈旧与缺失必须仍是两条路径 ---

// TestContinuationStaleAndMissingStayDistinct 陈旧绑定（409）与缺失绑定（410）必须
// 是两个不同的哨兵、两个不同的码，且都被真的产生。合并它们会把"续跑引用了别人
// 的响应 id"与"身份已变"混成一个响应。
func TestContinuationStaleAndMissingStayDistinct(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/proxy"})
	type contErr struct {
		statusSel string
		msg       string
	}
	decls := map[string]contErr{}
	uses := map[string]int{}
	for _, src := range srcs {
		if src.name != "continuation.go" {
			continue
		}
		for _, decl := range src.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				cl, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					// 取址复合字面量（&formatError{...}）包了一层一元表达式。
					ue, uok := vs.Values[0].(*ast.UnaryExpr)
					if !uok {
						continue
					}
					cl, ok = ue.X.(*ast.CompositeLit)
					if !ok {
						continue
					}
				}
				var ce contErr
				for _, elt := range cl.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					switch key.Name {
					case "status":
						se, ok := kv.Value.(*ast.SelectorExpr)
						if !ok {
							continue
						}
						pkg, ok := se.X.(*ast.Ident)
						if !ok {
							continue
						}
						ce.statusSel = pkg.Name + "." + se.Sel.Name
					case "msg":
						bl, ok := kv.Value.(*ast.BasicLit)
						if !ok || bl.Kind != token.STRING {
							continue
						}
						if s, uerr := strconv.Unquote(bl.Value); uerr == nil {
							ce.msg = s
						}
					}
				}
				decls[vs.Names[0].Name] = ce
			}
		}
	}
	stale, ok := decls["errContStale"]
	require.True(t, ok, "errContStale 必须存在")
	missing, ok := decls["errContNotFound"]
	require.True(t, ok, "errContNotFound 必须存在")
	require.Equal(t, "http.StatusConflict", stale.statusSel, "陈旧绑定 → 409")
	require.Equal(t, "http.StatusGone", missing.statusSel, "缺失绑定 → 410")
	require.NotEqual(t, stale.statusSel, missing.statusSel, "陈旧与缺失必须是两个码")
	require.Contains(t, strings.ToLower(stale.msg), "stale", "陈旧路径的文案须可辨识")
	require.Contains(t, strings.ToLower(missing.msg), "not found", "缺失路径的文案须可辨识")

	// 两者都必须被真的产生：声明之外至少各有一处引用。
	for _, src := range srcs {
		ast.Inspect(src.file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && (id.Name == "errContStale" || id.Name == "errContNotFound") {
				uses[id.Name]++
			}
			return true
		})
	}
	require.Greater(t, uses["errContStale"], 1, "errContStale 必须被产生（不能只剩声明）")
	require.Greater(t, uses["errContNotFound"], 1, "errContNotFound 必须被产生（不能只剩声明）")
}
