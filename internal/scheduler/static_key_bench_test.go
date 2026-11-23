// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// planKeyEqSink 是基准的汇：把比较结果存到包级变量，防止编译器把整轮比较当死代码消掉。
var planKeyEqSink bool

// BenchmarkPlanKeyEquality 是 planKey 的 `==` 的**常驻**基准（spec §9：键的比较是
// 预留/复用判据的热路径——每次预留尝试与每次"复用旧叶还是换新叶"判定都要比较整枚键）。
//
// 三条子基准对应判据的三种真实走向，全部只经唯一构造函数取键：
//   - equal：两侧事实完全相同——预留 fence（`fact.planKey == c.PlanKey`）与叶复用
//     （`staticKeyOf(old) == staticKeyOf(new)`）的**命中**路径，比较必须走完全部字段。
//     这是最常见的路径（每次编译对每个账号比较一次，绝大多数账号未变），也是本次
//     "两枚 string → 一枚 [32]byte"直接受益的那条。
//   - differ-credential：只有凭据面不同（上游鉴权键轮转）。本单元改的就是这一维；
//     重排后 credentialPairDigest 是 planKey 的**末字段**，所以这也是"最晚短路"的那条。
//   - differ-last-field：只有 groupIDsDigest（组归属变更）不同。重排前它是末字段、
//     重排后是末字段之前的一枚数组——**保留这个名字是为了 before/after 的标签可比**，
//     它测的是"在数组块尾部才短路"这一形状。
//
// **本文件必须能在改动前后两个提交上编译**：before/after 对照的做法是把同一个文件
// 拷进基提交的临时 worktree 跑一遍。因此这里只许用唯一构造函数 + 键比较，不得触摸
// planKey 的任何字段名——凭据面在改动里换了表示，字段名正是会变的那个东西。
func BenchmarkPlanKeyEquality(b *testing.B) {
	build := func(upstream, pat string, gids []int64) planKey {
		tpl := &domain.Template{
			ID:               3,
			BaseURL:          "https://tpl.example/v1",
			CredentialType:   credential.TypeAPIKey,
			StripImageTools:  true,
			Models:           []string{"m-b", "m-a"},
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"m-a", "m-b"}},
		}
		acc := domain.Account{
			ID: 7, Name: "acct-7", TemplateID: tpl.ID, UpstreamKey: upstream,
			MaxConcurrency: 4, Enabled: true, IdentityRevision: 5,
			Ext: &domain.AccountExt{CodexAccountID: new("acct-up"), CodexPATKey: new(pat)},
		}
		return planKeyOf(newSnapshotStatic(acc, tpl, gids, directEgress(), nil))
	}
	// 16 字节上游键 + 16 字节 pat：改前它们是 planKey 里两枚独立的 string 字段
	// （比较 = 长度检查 + memequal），改后是一枚 [32]byte。
	const upstream = "sk-live-0123456789ab"
	const pat = "pat-0000000000000016"

	b.Run("equal", func(b *testing.B) {
		left := build(upstream, pat, []int64{3, 9})
		right := build(upstream, pat, []int64{9, 3}) // 不同对象、不同切片实例、同事实
		if left != right {
			b.Fatal("benchmark fixture must produce equal keys; otherwise this sub-benchmark measures the wrong path")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			planKeyEqSink = left == right
		}
	})
	b.Run("differ-credential", func(b *testing.B) {
		left := build(upstream, pat, []int64{3, 9})
		right := build("sk-live-0123456789zz", pat, []int64{3, 9}) // 仅上游鉴权键不同
		if left == right {
			b.Fatal("benchmark fixture must produce unequal keys")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			planKeyEqSink = left == right
		}
	})
	b.Run("differ-last-field", func(b *testing.B) {
		left := build(upstream, pat, []int64{3, 9})
		right := build(upstream, pat, []int64{3, 10}) // 仅 groupIDsDigest（最后一个字段）不同
		if left == right {
			b.Fatal("benchmark fixture must produce unequal keys")
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			planKeyEqSink = left == right
		}
	})
}
