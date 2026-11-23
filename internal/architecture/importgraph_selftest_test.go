package architecture

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件用合成图驱动 checkEdges / matchesPrefix，不读真实仓库：
// 门禁逻辑本身必须能证明会红。前三个用例对应 brief §6 的三条，
// 后两个防「前缀写歪」与「未知包默认放行」。

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
