// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package paymentregistry

// contract_test.go 两条**跨协议**门禁，都放在本包：它是全仓唯一同时 import
// `paymentregistry` 与具体协议实现的包（门禁的规范表述：具体协议构造只能出现在
// 这里），因此也是"新增协议自动被覆盖"这句话唯一能被机器检查的落点。
//
//   - TestConfigSchemaAndParseConfigContract（P51，§18-6）：`Registry(nil).All()`
//     逐协议跑同一组双向判据。**新增协议不需要再写一条 P**：注册进 Registry 的那一刻
//     就进了本用例的子测试。深度判据（真实 PEM、逐字段删键）留在
//     `internal/service/epay/contract_test.go`，本文件只做协议无关的部分。
//   - TestNoSecretFieldsMethodDeclarations（P47⑤ / N46）：AST 扫面内不得存在名为
//     `SecretFields` 的函数或方法声明。
//
// 为什么必须用 `Registry(nil)` 构造：nil client 是协议构造函数的**契约**（N67），
// 表驱动与门禁派生协议名都走这条路——顺带把"构造函数容忍 nil client"钉住。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// 探针 config：schema 之外的键。严格 JSON（DisallowUnknownFields）下必然被拒；
// 若某协议的解析器"宽容"到接受任意键，说明它的写面校验和 schema 是两张皮。
const (
	probeEmptyConfig   = `{}`
	probeUnknownConfig = `{"__unknown_probe__":1}`
)

// TestRegistryToleratesNilClient 构造器契约（N67）：`Registry(nil)` 必须成功。
//
// 它同时是下面那条表驱动用例的前置条件——"用 nil 构造"不是测试的便利，
// 而是协议构造函数被要求的性质（`epay.New(nil)` 内部兜默认客户端）。
func TestRegistryToleratesNilClient(t *testing.T) {
	registry, err := Registry(nil)
	require.NoError(t, err, "协议构造函数必须容忍 nil http client（spec v2 §4 / N67）")
	require.NotNil(t, registry)
	require.NotEmpty(t, registry.All(),
		"本二进制至少要注册一个协议：0 个时下面那条表驱动门禁会空转成恒绿")
}

// orderingProvider 只用于"注册表定序"判据：Name() 是唯一有意义的成员，其余方法
// 只为满足 domain.PaymentProvider（本用例不建单、不收回调）。
type orderingProvider struct{ name domain.ProviderName }

var _ domain.PaymentProvider = orderingProvider{}

func (p orderingProvider) Name() domain.ProviderName { return p.name }

func (orderingProvider) Capabilities() []string { return nil }

func (orderingProvider) ConfigSchema() domain.ConfigSchema { return domain.ConfigSchema{} }

func (orderingProvider) ParseConfig([]byte) (domain.AccountConfig, error) {
	return nil, domain.ErrConfigMissing
}

func (orderingProvider) BuildPayURL(domain.AccountConfig, domain.PayRequest) (string, error) {
	return "", domain.ErrConfigMissing
}

func (orderingProvider) VerifyNotify(domain.AccountConfig, url.Values) (domain.NotifyEvent, error) {
	return domain.NotifyEvent{}, domain.ErrConfigMissing
}

// TestProviderRegistryOrdersByName 注册表是**唯一**定序点（spec v2 §4 / N55）：插入
// 序反着来也必须按 `name` 升序返回，且这个序与端点输出（P46）是同一个。
//
// 为什么要在这里（而不是只靠端点用例）钉一次：端点的用例依赖“至少两个真实协议”
// 才能观测到顺序，而本二进制当前只编译进一个协议（epay_v2）——单个元素时插入序恰好
// 等于升序。本用例用两个假协议把这条不变量**非空转**地钉住（M39 的杀手）。
func TestProviderRegistryOrdersByName(t *testing.T) {
	reverse, err := domain.NewProviderRegistry([]domain.PaymentProvider{
		orderingProvider{name: "zeta_proto"}, // 先插字典序大的
		orderingProvider{name: "alpha_proto"},
	})
	require.NoError(t, err)
	require.Equal(t, []domain.ProviderName{"alpha_proto", "zeta_proto"}, providerNames(reverse.All()),
		"All() 必须按 name 升序，与插入序无关（M39：退回插入序即红）")

	forward, err := domain.NewProviderRegistry([]domain.PaymentProvider{
		orderingProvider{name: "alpha_proto"}, // 插入序已是升序：不得被排序改动
		orderingProvider{name: "zeta_proto"},
	})
	require.NoError(t, err)
	require.Equal(t, providerNames(reverse.All()), providerNames(forward.All()),
		"两神插入序必须得到同一个枚举结果（顺序是注册表的属性，不是装配的巧合）")
}

// providerNames 抽名字序列（逐位比较用）。
func providerNames(providers []domain.PaymentProvider) []domain.ProviderName {
	out := make([]domain.ProviderName, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name())
	}
	return out
}

// TestConfigSchemaAndParseConfigContract P51：schema ↔ ParseConfig 的双向一致性。
// 每条子测试对**注册表里的每个协议**跑同一组判据（新增协议自动进入）：
//
//	① Fields[].Name 非空且两两不同（重复 = 表单画两个同名输入框，后写的覆盖前写的）；
//	② ParseConfig(`{}`) 失败（空对象不够：漏一个必需键就必须拒绝）；
//	③ ParseConfig(`{"__unknown_probe__":1}`) 失败（schema 之外的键被拒 —— 严格 JSON，
//	   否则"多写一个键"会静默生效，schema 与解析器的字段集合从此可以分叉）；
//	④ Secret==true 的字段集合 == SecretNames()（后者是**派生视图**，不是第二份声明）。
//
// ②③ 是"不能为空/不能多余"的下限判据；"schema 里的每个键都真的被接受"无法用
// 协议无关的值域构造（每家的私钥/密钥格式不同），故由各协议的深度用例承担
// （epay 见 internal/service/epay/contract_test.go 的第 5–7 条）。
func TestConfigSchemaAndParseConfigContract(t *testing.T) {
	registry, err := Registry(nil)
	require.NoError(t, err)
	protocols := registry.All()
	require.NotEmpty(t, protocols)

	for _, p := range protocols {
		t.Run(string(p.Name()), func(t *testing.T) {
			schema := p.ConfigSchema()

			// ① 字段名非空、两两不同。
			require.NotEmpty(t, schema.Fields, "协议必须至少声明一个配置字段")
			names := make([]string, 0, len(schema.Fields))
			seen := make(map[string]bool, len(schema.Fields))
			for i, f := range schema.Fields {
				require.NotEmpty(t, f.Name, "Fields[%d].Name 不得为空（空名 = 前端画不出输入框）", i)
				require.False(t, seen[f.Name], "字段名重复 %q（重复 = 两个同名输入框互相覆盖）", f.Name)
				seen[f.Name] = true
				names = append(names, f.Name)
			}

			// ② 空对象必须被拒。
			_, emptyErr := p.ParseConfig([]byte(probeEmptyConfig))
			require.Error(t, emptyErr, "ParseConfig({}) 必须失败：没有任何必需键的配置不可用")

			// ③ schema 之外的键必须被拒（严格 JSON）。
			_, unknownErr := p.ParseConfig([]byte(probeUnknownConfig))
			require.Error(t, unknownErr,
				"ParseConfig 必须拒绝 schema 之外的键（否则 schema 与解析器的字段集合可以静默分叉）")

			// ④ 秘密字段只有一个声明口：Fields[].Secret。
			secretInFields := make([]string, 0, 1)
			for _, f := range schema.Fields {
				if f.Secret {
					secretInFields = append(secretInFields, f.Name)
				}
			}
			require.Equal(t, secretInFields, schema.SecretNames(),
				"SecretNames() 必须逐位等于 Fields 里 Secret==true 的 Name（派生视图，不是第二份声明）")
			for _, name := range schema.SecretNames() {
				require.Contains(t, names, name, "SecretNames() 里的 %q 必须是 Fields 声明的字段", name)
				require.True(t, seen[name])
			}
		})
	}
}

// secretFieldsForbiddenDirs 是 `SecretFields` 的 AST 扫面（spec v2 §13 P47⑤ / N46）：
// 承载**支付协议面与掩码路径**的全部手写包，**递归**含其子包（`internal/handler` 下的
// payment / paymentadmin 与 `internal/service/epay` 都是掩码路径的一部分，单独列出
// 只为把"这些子包被有意覆盖"写成显式清单）。生成代码不在内（`*.gen.go` 由 openapi
// 生成器持有，其符号形态不由我们决定）。
var secretFieldsForbiddenDirs = []string{
	"internal/domain",
	"internal/service",
	"internal/service/epay",
	"internal/handler",
	"cmd/server",
}

// TestNoSecretFieldsMethodDeclarations P47⑤：`SecretFields` 不得复活。
//
// 为什么编译器拦不住：把方法从 `domain.PaymentProvider` 接口删掉之后，实现体上再留
// 一个同名的 `func (p *Provider) SecretFields() []string` **照样编译**（Go 不禁止
// 接口未使用的方法）。于是"唯一声明口"可以静默退化成两份：schema 喂前端、残存方法
// 喂掩码——两处一旦分叉，密钥的掩码与表单字段就会各说一套。
//
// 判据落在 `FuncDecl.Name`（含方法，Recv 非空也算），**不扫注释**：历史修订记录里
// 提到这个名字是合法的。
func TestNoSecretFieldsMethodDeclarations(t *testing.T) {
	root := moduleRoot(t)
	scanned := 0
	var offenders []string
	visited := make(map[string]bool, len(secretFieldsForbiddenDirs))
	for _, dir := range secretFieldsForbiddenDirs {
		// 递归：`internal/service` 会连带 `internal/service/epay`，两个根可能重叠，
		// 故按绝对路径去重（同一份文件只扫一次，offenders 不重复计）。
		walkErr := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				// 生成代码与前端产物不在 AST 面内。
				if entry.Name() == "dist" || entry.Name() == "web" {
					return filepath.SkipDir
				}
				return nil
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			if visited[path] {
				return nil
			}
			visited[path] = true
			fset := token.NewFileSet()
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			scanned++
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Name != nil && fn.Name.Name == "SecretFields" {
					rel, relErr := filepath.Rel(root, path)
					require.NoError(t, relErr)
					offenders = append(offenders, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		require.NoError(t, walkErr, "扫面目录必须存在：%s", dir)
	}
	// 反向自检：扫面真的读到了文件（否则"没找到"是空转，门禁形同虚设）。
	require.Greater(t, scanned, 50, "AST 扫面必须覆盖到全部目标包（读到 %d 个文件）", scanned)
	require.Empty(t, offenders,
		"`SecretFields` 是 r5 的协议方法名，已被 ConfigSchema 取代；扫描面内不得存在同名 FuncDecl（改成 ConfigSchema/SecretNames）")
}

// moduleRoot 定位模块根：本文件在 <root>/internal/paymentregistry/ 下，向上找到
// 含 go.mod 的目录。找不到即 FailNow——门禁绝不能在"路径猜错"时静默通过。
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "找不到模块根（go.mod）")
		dir = parent
	}
}
