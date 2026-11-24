// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 出口池的**消费方端口**与账号出口的**唯一解析点**（P5-3-B，账本 576）。
//
// 为什么端口声明在这里而不是 pkg/egress：方向必须从消费方指向实现。`pkg/egress.Pool`
// 若自己导出接口，`pkg` 就得知道"谁在消费我"；反过来，池凭**结构**满足本接口，
// 实现包一行不用改（与 scheduler.ClientResources 同一手法，P5-1）。
//
// 本文件也是"未接出口"这件事唯一的语义定义点：池为 nil 时**不再补一个空池**（那会
// 让"没注入"看起来像"注入了"），而是按账号是否绑定出口分成两条明确的路：未绑定 ⇒
// 直连是确定状态；绑定 ⇒ 装配违例，装载期直接报错。

import (
	"fmt"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egressapi"
)

// EgressPool 是出口池在本包的全部需求面——四个方法，一个不多。
//
//   - Direct / For 在**快照期**解析出口（buildSnapshots、accountRuntime.fullLocked）；
//   - Size / Detach 在**配置面**回收不再被引用的规格 client（pruneEgress）。
//
// 热路径一次都不调：请求只读快照上已解析好的 *egressapi.Client 指针，所以接口间接
// 调用不进每请求路径（`-gcflags=-m` 的结论见 P5-3-B 报告）。
type EgressPool interface {
	// Direct 直连出口（未绑定账号的出口；构造期单例，恒非 nil）。
	Direct() *egressapi.Client
	// For 按规格取/建出口 client（同键复用同一指针，连接池因此跨快照保留）。
	For(spec egressapi.Spec) *egressapi.Client
	// Detach 摘除并**返回**不再被 active 引用的规格 client（锁内只摘，关闭由调用方
	// 在锁外做）。
	Detach(active map[[16]byte]struct{}) []*egressapi.Client
	// Size 当前注册的规格 client 数（不含直连）；零代理部署据此跳过回收扫描。
	Size() int
}

// unwiredDirectEgress 是"出口池未注入"时的直连出口：默认传输，两个口味同一个
// client（http.DefaultClient）。
//
// 它复现的是被删掉的 `egress.NewPool(nil).Direct()` 在 nil 底座下的**实际形状**，
// 不是在本包重新发明"直连怎么派生"：NewPool(nil) 会把 base 落到 http.DefaultClient，
// 而 codexClientOf 对没有 Transport 的底座原样返回底座（pkg/egress/pool.go），于是
// 两个口味本来就是同一个 http.DefaultClient。代理规格的 transport 派生仍然只有实现
// 包一处；这里只是一个**没有底座**时的零值出口。
//
// 边界（写死）：只服务**未绑定**账号。绑定账号在 egressClientFor 里直接 fail closed，
// 绝不到这里——那正是"未接出口"不能静默变成"直连"的地方。
var unwiredDirectEgress = egressapi.NewClient(egressapi.Spec{}, [egressapi.NumFlavours]*http.Client{
	egressapi.FlavourHTTP:          http.DefaultClient,
	egressapi.FlavourNoCompression: http.DefaultClient,
})

// accountEgressBound 报告账号是否**绑定了**出口：ProxyID 是落库的绑定事实（代理被
// 软删/展开失败时 Proxy 为空而绑定仍在），Proxy 是装载器展开后的规格。两者任一非空
// 即绑定——只看 Proxy 会把"绑定但展开缺失"误判成直连（那正是 spec §4.7 的 L8）。
func accountEgressBound(acc *domain.Account) bool {
	return acc != nil && (acc.ProxyID != nil || acc.Proxy != nil)
}

// egressConfigSkip 报告账号在**配置面**（accountRuntime 的 universe）上是否该被跳过：
// 绑定但未展开（ProxyID 在、Proxy 为空；spec §4.7 L8），**且池已注入**。
//
// 为什么是"跳过"而不是"报错"：这个形状是**运行期数据事实**——代理被软删/缺失时
// 装载器会保留 ProxyID、把 Proxy 留空（repository 的 expandAccountProxy），一次软删
// 就会造出来，它不是装配错误。报错意味着一个陈旧绑定把整批装载打掉（fullLocked 返回
// error ⇒ 整组配置不发布），是可用性回退。而跳过与 routableFor 的结论一致：它本来就
// 不可派发，永远不会成为请求出口，所以"配置面上没有出口"才是这个账号的真话。
//
// 池未注入时**不跳过**：那一侧的判据是"有绑定就必须有池"，由 egressClientFor 报
// ErrEgressNotWired——陈旧绑定不能把既有判据吃掉。
func egressConfigSkip(pool EgressPool, acc *domain.Account) bool {
	return pool != nil && accountEgressBound(acc) && acc.Proxy == nil
}

// egressNotWiredErr 是装配违例的**唯一**错误构造点：账号 + 绑定代理 id（ProxyID 为
// 空时只报账号，说明绑定来自未落库的规格）。
func egressNotWiredErr(acc *domain.Account) error {
	if acc == nil {
		return ErrEgressNotWired
	}
	if acc.ProxyID != nil {
		return fmt.Errorf("%w: account %d → proxy %d", ErrEgressNotWired, acc.ID, *acc.ProxyID)
	}
	return fmt.Errorf("%w: account %d → proxy spec", ErrEgressNotWired, acc.ID)
}

// egressClientFor 是账号出口的**唯一解析点**：快照期（buildSnapshots）与配置期
// （accountRuntime.fullLocked）都走这里，两条路径不可能对"绑定/未装配"给出不同结论。
//
// 池未注入（nil）不是"补一个空池"，而是两条不同的事实：
//
//   - 未绑定账号：直连是**确定状态**，用 unwiredDirectEgress（默认传输，与删掉的空
//     底座池同形）——"没配代理"的部署照常工作；
//   - 绑定账号：**装配违例**，返回 ErrEgressNotWired，绝不退化成直连（那会让"配了
//     代理"的账号从别人的 IP 出栈，且没人知道）。
//
// 池在、但给不出 client（接口契约违约）同样按装配违例处理：数据面 `sel.Egress.HTTP()`
// 是裸调用，宁可装载期报错，也不把 nil 指针发给热路径。
//
// **"绑定但未展开"（ProxyID != nil && Proxy == nil，spec §4.7 L8）的可达性边界**——
// 这个形状在真实数据里会出现（`repository` 的 expandAccountProxy 在代理被软删/缺失时
// 保留 ProxyID、把 Proxy 留成 nil）。谁见得到它：
//
//   - 快照面（buildSnapshots）：**见得到**。它按组枚举**全部**账号、不过滤 routable，
//     所以这里会给该账号解析出一个直连 client。但结果落在一片 routable=false 的叶子上
//     （routableFor 对这个形状恒 false），候选过滤永不选中它 ⇒ 这个指针**永远不会成为
//     任何请求的出口**，危害面为零。快照面因此不额外处置：跳过它要在快照面再复制一份
//     "L8 ⇒ 无出口"的策略，而那里唯一的消费者是请求路径。
//   - 配置面（accountRuntime.fullLocked）：**见得到，且必须处置**。它的 universe 直接
//     变成传输资源表的保留键与 prune 的 active 集，"绑定账号持直连"会作为**配置事实**
//     被登记（见 egressConfigSkip：池已注入时跳过该账号；池未注入时不跳过，仍报
//     ErrEgressNotWired）。
//
// 两条规则不冲突，因为它们回答两个不同的问题：叶子需要一片可读的出口指针（哪怕永不
// 使用），配置需要一句真话（这个账号此刻没有出口）。
func egressClientFor(pool EgressPool, acc *domain.Account) (*egressapi.Client, error) {
	if pool == nil {
		if accountEgressBound(acc) {
			return nil, egressNotWiredErr(acc)
		}
		return unwiredDirectEgress, nil
	}
	eg := pool.Direct()
	if acc != nil && acc.Proxy != nil {
		eg = pool.For(specOfDomain(acc.Proxy))
	}
	if eg == nil {
		return nil, egressNotWiredErr(acc)
	}
	return eg, nil
}
