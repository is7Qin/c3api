package architecture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件用合成图驱动 checkEdges / matchesPrefix / dirsWithGoFiles / 指纹函数，
// 不读真实仓库：门禁逻辑本身必须能证明会红。

// writeGoFile 造一个只含 `package x` 的 .go 文件，父目录自动创建。
func writeGoFile(t *testing.T, root string, parts ...string) {
	t.Helper()
	p := filepath.Join(append([]string{root}, parts...)...)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte("package x\n"), 0o600))
}

func TestImportGraphSelfTestRejectsNewEdge(t *testing.T) {
	pol := policy{rules: []allowRule{
		{from: "internal/a", to: []string{"internal/b"}},
	}}
	graph := []edge{
		{from: "internal/a", to: "internal/b"},
		{from: "internal/a", to: "internal/c"}, // 不在 allow，也不在账本
	}

	got := checkEdges(graph, pol, nil)
	require.Len(t, got, 1)
	require.Equal(t, kindNewViolation, got[0].kind)
	require.Equal(t, "internal/a -> internal/c", got[0].edge.String())
	// 失败信息必须带 from -> to、判定词，以及「这条 from 到底允许什么」。
	require.Contains(t, got[0].String(), "new violation")
	require.Contains(t, got[0].String(), "internal/a -> internal/c")
	require.Contains(t, got[0].String(), "只允许")

	// pkg/* -> internal/* 是硬红线：没有 legacy 通道，登记了也照红。
	hard := checkEdges([]edge{{from: "pkg/x", to: "internal/a"}},
		policy{rules: []allowRule{
			{from: "pkg/x", all: true},
			{from: "internal/a"},
		}},
		map[edge]legacyEntry{
			{from: "pkg/x", to: "internal/a"}: {reason: "不该存在的豁免", phase: phaseKeep},
		})
	require.Len(t, hard, 1)
	require.Equal(t, kindHardViolation, hard[0].kind)
	require.Contains(t, hard[0].String(), "pkg/x -> internal/a")
}

func TestImportGraphSelfTestRejectsStaleLegacy(t *testing.T) {
	pol := policy{rules: []allowRule{
		{from: "internal/a", to: []string{"internal/b"}},
	}}

	// 账本里有、图上没有 => stale。
	stale := map[edge]legacyEntry{
		{from: "internal/a", to: "internal/gone"}: {reason: "已被删除的旧边", phase: phaseP4},
	}
	got := checkEdges([]edge{{from: "internal/a", to: "internal/b"}}, pol, stale)
	require.Len(t, got, 1)
	require.Equal(t, kindStaleLegacy, got[0].kind)
	require.Equal(t, "internal/a -> internal/gone", got[0].edge.String())
	require.Contains(t, got[0].String(), "stale legacy")
	require.Contains(t, got[0].String(), "P4")

	// 边还在，但已经符合目标方向（被 allow 覆盖）=> 也算 stale，账本只能变短。
	redundant := map[edge]legacyEntry{
		{from: "internal/a", to: "internal/b"}: {reason: "早已合规的旧条目", phase: phaseP4},
	}
	got = checkEdges([]edge{{from: "internal/a", to: "internal/b"}}, pol, redundant)
	require.Len(t, got, 1)
	require.Equal(t, kindRedundantLegacy, got[0].kind)
}

func TestImportGraphSelfTestAcceptsAllowedGraph(t *testing.T) {
	pol := policy{
		rules: []allowRule{
			{from: "internal/a", to: []string{"internal/b", "pkg/"}},
			{from: "internal/ent", to: []string{"internal/ent/"}},
		},
		fallback: allowRule{from: "<叶子层>", to: []string{"internal/domain"}},
	}
	graph := []edge{
		{from: "internal/a", to: "internal/b"},
		{from: "internal/a", to: "pkg/logx"},
		{from: "internal/z", to: "internal/domain"},    // 落到兜底叶子规则
		{from: "internal/ent/x", to: "internal/ent/y"}, // ent 自引用跳过
		{from: "pkg/logx", to: "pkg/httpx"},            // pkg 之间自由依赖
	}

	require.Empty(t, checkEdges(graph, pol, nil))

	// 同一张图上多一条 allow 之外的边，必须立刻红。
	bad := append(append([]edge(nil), graph...), edge{from: "internal/z", to: "internal/a"})
	got := checkEdges(bad, pol, nil)
	require.Len(t, got, 1)
	require.Equal(t, kindNewViolation, got[0].kind)
	require.Equal(t, "internal/z -> internal/a", got[0].edge.String())
}

func TestImportGraphSelfTestUnknownPackageDenied(t *testing.T) {
	// 未声明层级的包落到 leafRule：它不许直接摸 repository / scheduler。
	pol := productionPolicy()
	got := checkEdges([]edge{
		{from: "internal/brandnew", to: "internal/repository"},
		{from: "internal/brandnew", to: "internal/domain"},
	}, pol, nil)
	require.Len(t, got, 1)
	require.Equal(t, kindNewViolation, got[0].kind)
	require.Equal(t, "internal/brandnew -> internal/repository", got[0].edge.String())
	require.Contains(t, got[0].String(), "<未显式声明：叶子层>")
}

func TestImportGraphSelfTestPrefixMatching(t *testing.T) {
	// 整包前缀：命中自身与子树，不命中同名前缀的别的包。
	require.True(t, matchesPrefix("internal/domain", "internal/domain"))
	require.True(t, matchesPrefix("internal/handler/user", "internal/handler"))
	require.False(t, matchesPrefix("internal/domainx", "internal/domain"))
	require.False(t, matchesPrefix("internal/handlerevil", "internal/handler"))

	// 显式子树前缀（以 / 结尾）只按字节前缀匹配。
	require.True(t, matchesPrefix("internal/handler/user", "internal/handler/"))
	require.True(t, matchesPrefix("pkg/logx", "pkg/"))
	require.False(t, matchesPrefix("internal/handler", "internal/handler/"))

	// 空前缀不得当成「全放行」。
	require.False(t, matchesPrefix("internal/domain", ""))
}

// TestImportGraphSelfTestDirsWithGoFilesSkipsOpaqueDirs 钉住「任意深度的
// testdata / tmp / vendor / node_modules 都不深入」：嵌套夹具目录若被当成
// 「含 .go 但未登记」的目录，TestImportGraphScanCoverage 就会变成假失败。
func TestImportGraphSelfTestDirsWithGoFilesSkipsOpaqueDirs(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, root, "a", "testdata", "x.go")
	writeGoFile(t, root, "a", "b", "tmp", "y.go")
	writeGoFile(t, root, "a", "b", "c", "vendor", "z.go")
	writeGoFile(t, root, "a", "b", "c", "node_modules", "w.go")
	// 对照组：普通目录必须照旧被看到，防止「跳过」写过头导致静默漏包。
	writeGoFile(t, root, "a", "b", "real", "x.go")

	dirs, err := dirsWithGoFiles(root)
	require.NoError(t, err)
	require.Equal(t, []string{"a/b/real"}, dirs,
		"嵌套 testdata/tmp/vendor/node_modules 必须整体跳过，普通目录必须保留")
}

// TestImportGraphSelfTestDirsWithGoFilesKeepsRootDeclaredDirs 反过来钉住根级：
// 根级 testdata / tmp / vendor 仍要被遍历看到（unscannedDirs 的「声明了就必须
// 存在」检查依赖这一点），但同样不深入；node_modules 不在 unscannedDirs 里，
// 根级也不登记。
func TestImportGraphSelfTestDirsWithGoFilesKeepsRootDeclaredDirs(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, root, "testdata", "a.go")
	writeGoFile(t, root, "tmp", "b.go")
	writeGoFile(t, root, "vendor", "github.com", "x", "c.go")
	writeGoFile(t, root, "node_modules", "d.go")
	writeGoFile(t, root, "internal", "real", "e.go")

	dirs, err := dirsWithGoFiles(root)
	require.NoError(t, err)
	require.Equal(t, []string{"internal/real", "testdata", "tmp", "vendor"}, dirs)
}

// TestImportGraphSelfTestFingerprintIsSensitive 证明指纹机制本身有效：
// 元素顺序变化必须不影响指纹（否则每次格式化都红），而每一种「加宽」方向
// 都必须改变指纹——这是 tripwire 的全部价值所在。
func TestImportGraphSelfTestFingerprintIsSensitive(t *testing.T) {
	base := policy{
		rules: []allowRule{
			{from: "internal/a", to: []string{"internal/b", "pkg/logx"}},
			{from: "internal/c", all: true, deny: []string{"internal/a"}},
		},
		fallback: leafRule,
	}
	fp := policyFingerprint(base)

	// 规则顺序、to 内部顺序都不承载语义，不能改变指纹。
	reordered := policy{
		rules: []allowRule{
			{from: "internal/c", all: true, deny: []string{"internal/a"}},
			{from: "internal/a", to: []string{"pkg/logx", "internal/b"}},
		},
		fallback: leafRule,
	}
	require.Equal(t, fp, policyFingerprint(reordered), "顺序不应影响指纹")
	require.Len(t, fp, 64, "指纹必须是 sha256 的十六进制")

	// 每一个加宽方向都必须让指纹变化（否则放宽矩阵就是零信号）。
	widened := map[string]policy{
		"加宽一条 to（billing 例）": {
			rules: []allowRule{
				{from: "internal/a", to: []string{"internal/b", "pkg/logx", "internal/billing"}},
				{from: "internal/c", all: true, deny: []string{"internal/a"}},
			},
			fallback: leafRule,
		},
		"新增一条 from": {
			rules: []allowRule{
				{from: "internal/a", to: []string{"internal/b", "pkg/logx"}},
				{from: "internal/c", all: true, deny: []string{"internal/a"}},
				{from: "internal/newbie", to: []string{"internal/domain"}},
			},
			fallback: leafRule,
		},
		"to 白名单换成 all": {
			rules: []allowRule{
				{from: "internal/a", all: true, to: []string{"internal/b", "pkg/logx"}},
				{from: "internal/c", all: true, deny: []string{"internal/a"}},
			},
			fallback: leafRule,
		},
		"多一个 deny（deny 也是判定的一部分）": {
			rules: []allowRule{
				{from: "internal/a", to: []string{"internal/b", "pkg/logx"}},
				{from: "internal/c", all: true, deny: []string{"internal/a", "internal/d"}},
			},
			fallback: leafRule,
		},
		"放宽 leafRule": {
			rules: []allowRule{
				{from: "internal/a", to: []string{"internal/b", "pkg/logx"}},
				{from: "internal/c", all: true, deny: []string{"internal/a"}},
			},
			fallback: allowRule{from: leafRule.from, to: []string{"internal/domain", "internal/repository"}},
		},
	}
	for name, pol := range widened {
		require.NotEqualf(t, fp, policyFingerprint(pol), "%s 必须改变指纹", name)
	}
}

// TestImportGraphSelfTestAllRuleCap 证明 all 上限真的会红：少一条、多一条都必须失败，
// 且只数 all: true（to 白名单不计入）。
func TestImportGraphSelfTestAllRuleCap(t *testing.T) {
	pol := policy{rules: []allowRule{
		{from: "internal/a", all: true},
		{from: "internal/b", all: true},
		{from: "internal/c", to: []string{"internal/a"}},
	}}
	require.Equal(t, 2, countAllRules(pol), "只数 all: true，不数 to 白名单")
	require.NoError(t, checkAllCap(pol, 2))
	require.ErrorContains(t, checkAllCap(pol, 1), "只允许改小")
	require.ErrorContains(t, checkAllCap(pol, 3), "只允许改小")
}
