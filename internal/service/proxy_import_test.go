// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// spec-proxy-import §6：批量导入的服务层判据。
//
// 纪律：只用 require；不并行（fakeStore 是共享可变状态）；失效次数用 invRecorder 数
// **templates** 类调用（invalidateProxyPool 的唯一 seam，见 proxy.go）。

// importRow 造一条合法导入行：只有 name 变化（唯一域是名字，host 不必跟着变）。
func importRow(name string) ProxyInput {
	return ProxyInput{Name: name, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.0.1", Port: 1080}
}

// TestImportProxiesPartialSuccess 部分成功：失败的行不影响其余行，结果与请求**等长同序**，
// 每行自带名字与错误分类所需的信息。
func TestImportProxiesPartialSuccess(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()
	// 预置一条占名的条目：导入里同名的行必须报 conflict（唯一域 = 每归属；这里是平台池）。
	_, err := svc.CreateProxy(ctx, validProxyInput())
	require.NoError(t, err)

	out, err := svc.ImportProxies(ctx, []ProxyInput{
		{Name: "imp-1", Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.0.1", Port: 1080},
		{Name: "imp-2", Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.0.2", Port: 70000}, // port 越界
		{Name: "p1", Scheme: domain.ProxySchemeHTTP, Host: "10.1.0.3", Port: 8080},       // 与预置条目重名
	})
	require.NoError(t, err, "逐行失败不进 error（请求本身合法）")
	require.Len(t, out, 3, "结果必须与请求等长，行序不能少也不能并")

	require.NoError(t, out[0].Err)
	require.Equal(t, 0, out[0].Index, "Index = 请求下标")
	require.Equal(t, "imp-1", out[0].Name, "成功行带归一后的名字")
	require.NotZero(t, out[0].ID)

	require.ErrorIs(t, out[1].Err, ErrInvalidInput)
	require.Equal(t, 1, out[1].Index)
	require.Equal(t, "imp-2", out[1].Name, "失败行也带名字（界面靠它对照原文）")
	require.Zero(t, out[1].ID)
	// 字段级说明（spec §4）：导入界面里"invalid input"一句话没法定位是哪一格。
	require.Contains(t, out[1].Err.Error(), "port")

	require.ErrorIs(t, out[2].Err, ErrConflict)
	require.Equal(t, 2, out[2].Index)
	require.Equal(t, "p1", out[2].Name)

	// 落库事实：只有第 0 行进去了（预置那条 + 新的一条）。
	list, err := svc.ListProxies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, out[0].ID, list[1].ID)
}

// TestImportProxiesInvalidatesOnce 整批**一次**失效：这是这条端点存在的真正理由
// （N 个单条 POST 就是 N 次全量调度重载）；全失败时一次都不该有。
func TestImportProxiesInvalidatesOnce(t *testing.T) {
	t.Run("全成功 ⇒ 恰好一次", func(t *testing.T) {
		svc, _, rec := newProxyTestService()
		out, err := svc.ImportProxies(context.Background(), []ProxyInput{
			importRow("a1"), importRow("a2"), importRow("a3"),
		})
		require.NoError(t, err)
		for _, o := range out {
			require.NoError(t, o.Err)
		}
		require.Equal(t, 1, rec.countKind("templates"), "3 行只该触发一次重载")
	})

	t.Run("全失败 ⇒ 一次都没有", func(t *testing.T) {
		svc, _, rec := newProxyTestService()
		bad := func(n string) ProxyInput {
			return ProxyInput{Name: n, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.0.9", Port: 0}
		}
		out, err := svc.ImportProxies(context.Background(), []ProxyInput{bad("b1"), bad("b2")})
		require.NoError(t, err)
		require.Len(t, out, 2)
		for _, o := range out {
			require.ErrorIs(t, o.Err, ErrInvalidInput)
		}
		require.Zero(t, rec.total(), "没有任何一行成功 ⇒ 不该触发重载（没变的东西不重载）")
	})

	t.Run("部分成功 ⇒ 恰好一次", func(t *testing.T) {
		svc, _, rec := newProxyTestService()
		out, err := svc.ImportProxies(context.Background(), []ProxyInput{
			importRow("c1"),
			{Name: "c2", Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.0.9", Port: 0},
		})
		require.NoError(t, err)
		require.NoError(t, out[0].Err)
		require.ErrorIs(t, out[1].Err, ErrInvalidInput)
		require.Equal(t, 1, rec.countKind("templates"))
	})
}

// TestImportProxiesLimits 上限（spec §2）：0 行与超上限都是**请求不合法**（400），
// 且不产生任何写入。
func TestImportProxiesLimits(t *testing.T) {
	svc, _, rec := newProxyTestService()
	ctx := context.Background()

	_, err := svc.ImportProxies(ctx, nil)
	require.ErrorIs(t, err, ErrInvalidInput, "空数组是调用方 bug，不是 no-op")

	tooMany := make([]ProxyInput, proxyImportMaxRows+1)
	for i := range tooMany {
		tooMany[i] = importRow(fmt.Sprintf("x%d", i))
	}
	_, err = svc.ImportProxies(ctx, tooMany)
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Contains(t, err.Error(), fmt.Sprintf("1..%d", proxyImportMaxRows))

	list, err := svc.ListProxies(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "被拒的请求不得落库")
	require.Zero(t, rec.total(), "被拒的请求不得触发重载")

	// 恰好等于上限 ⇒ 合法（边界不是 off-by-one）。
	ok := make([]ProxyInput, proxyImportMaxRows)
	for i := range ok {
		ok[i] = importRow(fmt.Sprintf("y%d", i))
	}
	out, err := svc.ImportProxies(ctx, ok)
	require.NoError(t, err)
	require.Len(t, out, proxyImportMaxRows)
	for _, o := range out {
		require.NoError(t, o.Err)
	}
}

// TestImportProxiesSupplierScope 供应商面：私网行只让**那一行**失败，其余照建；
// 归属恒为本人（写体里没有 owner_uid，归属只由作用域决定）。
func TestImportProxiesSupplierScope(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(41))

	out, err := svc.ImportProxies(ctx, []ProxyInput{
		{Name: "s-pub", Scheme: domain.ProxySchemeSOCKS5, Host: "203.0.113.10", Port: 1080},
		{Name: "s-priv", Scheme: domain.ProxySchemeSOCKS5, Host: "10.0.0.5", Port: 1080},
	})
	require.NoError(t, err)
	require.NoError(t, out[0].Err)
	require.ErrorIs(t, out[1].Err, ErrInvalidInput, "供应商自建出口的私网拒绝逐行生效")

	list, err := svc.ListProxies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "s-pub", list[0].Name)
	require.NotNil(t, list[0].OwnerUID)
	require.Equal(t, int64(41), *list[0].OwnerUID, "归属恒由作用域钉死")
}

// TestImportProxiesDefaults 与单条创建同一条 prepare 路径：enabled 缺省 true、显式 false
// 生效、remark 去首尾空白后落值。
func TestImportProxiesDefaults(t *testing.T) {
	svc, _, _ := newProxyTestService()
	ctx := context.Background()
	disabled := false
	remark := "  from paste  "
	out, err := svc.ImportProxies(ctx, []ProxyInput{
		importRow("d1"),
		{Name: "d2", Scheme: domain.ProxySchemeHTTP, Host: "10.1.0.8", Port: 8080, Enabled: &disabled, Remark: &remark},
	})
	require.NoError(t, err)
	require.NoError(t, out[0].Err)
	require.NoError(t, out[1].Err)

	list, err := svc.ListProxies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.True(t, list[0].Enabled, "缺省 enabled=true（与单条创建同）")
	require.False(t, list[1].Enabled, "显式 false 生效")
	require.Equal(t, "from paste", list[1].Remark, "remark 与单条创建同样 trim")
}

// TestImportProxiesMatchesCreateProxy 同一行入参走两条入口，结果必须一致——否则
// "批量"与"单条"就有了两套判据（这条断言的就是"没有第二套"）。
func TestImportProxiesMatchesCreateProxy(t *testing.T) {
	in := ProxyInput{Name: "same", Scheme: domain.ProxySchemeHTTPS, Host: "h.example.com", Port: 3128}

	single, _, _ := newProxyTestService()
	_, singleErr := single.CreateProxy(context.Background(), in)

	batch, _, _ := newProxyTestService()
	out, err := batch.ImportProxies(context.Background(), []ProxyInput{in})
	require.NoError(t, err)
	require.Len(t, out, 1)

	require.NoError(t, singleErr)
	require.NoError(t, out[0].Err)

	singleList, err := single.ListProxies(context.Background())
	require.NoError(t, err)
	batchList, err := batch.ListProxies(context.Background())
	require.NoError(t, err)
	require.Len(t, singleList, 1)
	require.Len(t, batchList, 1)
	require.Equal(t, singleList[0], batchList[0], "两条入口必须产出一模一样的行")

	// 非法入参同样必须同码（含字段级说明的形状）。
	bad := ProxyInput{Name: "same", Scheme: domain.ProxySchemeHTTPS, Host: "h.example.com", Port: 0}
	_, singleErr = single.CreateProxy(context.Background(), bad)
	out, err = batch.ImportProxies(context.Background(), []ProxyInput{bad})
	require.NoError(t, err)
	require.ErrorIs(t, singleErr, ErrInvalidInput)
	require.ErrorIs(t, out[0].Err, ErrInvalidInput)
	require.Equal(t, singleErr.Error(), out[0].Err.Error(), "错误文本也要同源（同一个归一函数）")
	require.True(t, strings.Contains(out[0].Err.Error(), "port"))
}
