// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identitypool

// 布局契约（P5b v2 修正 5）：`type + size + offset` 全打进 testdata/layout.golden，
// 逐字节比。为什么连 offset 一起钉：等体积重排（字段顺序换了、缓存行归属变了）
// `unsafe.Sizeof` 看不见，但它会改热路径的伪共享行为。
//
// 基线（c6cb3250 上 momus 实测，搬包后必须一致）：
//
//	Session       40  busy=0  h=8   threadKey=16  threads=32
//	Pool          88  slots=0 cursor=24 installationID=32 accountID=48
//	                  policy=56 retireThread=72 hooks=80
//	Spec          56  Registry 8  Reclaim 24  ThreadRetire 24
//
// 所以"加注入点"这件事必须**零布局代价**地做：五个钩子收进一个 `*hooks`
// （正好占住原 `reserveHook` 的那 8 字节），I1 的计数器放测试侧旁表而不是 Session。

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// layoutDump 把六个契约类型的布局摊成稳定文本（reflect 顺序 = 声明顺序）。
func layoutDump() string {
	types := []struct {
		name   string
		sample any
	}{
		{"Session", Session{}},
		{"Pool", Pool{}},
		{"Spec", Spec{}},
		{"Registry", Registry{}},
		{"Reclaim", Reclaim{}},
		{"ThreadRetire", ThreadRetire{}},
	}
	var b strings.Builder
	for _, ty := range types {
		t := reflect.TypeOf(ty.sample)
		fmt.Fprintf(&b, "%s size=%d align=%d\n", ty.name, t.Size(), t.Align())
		for i := range t.NumField() {
			f := t.Field(i)
			fmt.Fprintf(&b, "  %s %s offset=%d size=%d\n", f.Name, f.Type.String(), f.Offset, f.Type.Size())
		}
	}
	return b.String()
}

// TestLayoutGolden 布局零 diff：golden 是契约文件，改它必须是一次显式的评审动作
// （`git diff -- testdata/layout.golden` 必须为空）。
func TestLayoutGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "layout.golden"))
	require.NoError(t, err, "布局基线缺失：testdata/layout.golden 是布局契约，不得删除")
	got := layoutDump()
	// 唯一的重写通道：显式环境变量（评审可见的一次动作），平时恒为只读比对。
	if os.Getenv("IDENTITYPOOL_LAYOUT_UPDATE") == "1" {
		require.NoError(t, os.WriteFile(filepath.Join("testdata", "layout.golden"), []byte(got), 0o644))
		t.Log("已重写 testdata/layout.golden")
		return
	}
	require.Equal(t, string(want), got,
		"布局漂移（等体积重排也算漂移）：若是有意的布局变更，必须同时更新 testdata/layout.golden "+
			"并在评审里说明伪共享影响")
}

// TestLayoutPinnedToBaseline 把"与 c6cb3250 一致"这件事写进代码（不只躺在 golden
// 文件里）：谁改大/改乱这两个热路径类型，这里直接指出改了哪一格。
func TestLayoutPinnedToBaseline(t *testing.T) {
	session := reflect.TypeOf(Session{})
	require.Equal(t, uintptr(40), unsafe.Sizeof(Session{}), "Session 必须保持 40B（I1 计数器不得进 Session）")
	require.Equal(t, uintptr(0), session.Field(0).Offset, "Session.busy 偏移")
	require.Equal(t, uintptr(8), session.Field(1).Offset, "Session.h 偏移")
	require.Equal(t, uintptr(16), session.Field(2).Offset, "Session.threadKey 偏移")
	require.Equal(t, uintptr(32), session.Field(3).Offset, "Session.threads 偏移")
	require.Equal(t, 4, session.NumField(), "Session 字段数（多一个字段就会改布局）")

	pool := reflect.TypeOf(Pool{})
	require.Equal(t, uintptr(88), unsafe.Sizeof(Pool{}), "Pool 必须保持 88B（注入点收在 *hooks 里）")
	for name, want := range map[string]uintptr{
		"slots": 0, "cursor": 24, "installationID": 32, "accountID": 48,
		"policy": 56, "retireThread": 72, "hooks": 80,
	} {
		f, ok := pool.FieldByName(name)
		require.True(t, ok, "Pool.%s 不存在（布局契约里它必须在）", name)
		require.Equal(t, want, f.Offset, "Pool.%s 偏移", name)
	}
	require.Equal(t, 7, pool.NumField(), "Pool 字段数")

	require.Equal(t, uintptr(56), unsafe.Sizeof(Spec{}), "Spec 必须保持 56B（= 原 poolSpec）")
	require.Equal(t, uintptr(8), unsafe.Sizeof(Registry{}), "Registry 必须保持 8B（只有一个 map 头）")
	require.Equal(t, uintptr(24), unsafe.Sizeof(Reclaim{}), "Reclaim 必须保持 24B（一个切片头）")
	require.Equal(t, uintptr(24), unsafe.Sizeof(ThreadRetire{}), "ThreadRetire 必须保持 24B")
}
