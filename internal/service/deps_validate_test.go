// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/notify"
)

// nilViewLoader 只用来构造「接口里装 nil 指针」的装箱陷阱。
type nilViewLoader struct{}

func (l *nilViewLoader) Reload(context.Context) bool { return l != nil }

func TestDepsValidateRequired(t *testing.T) {
	// 零值 Deps：两个必选都缺，错误必须同时指名，不能只报第一个。
	err := Deps{}.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "Store")
	require.Contains(t, err.Error(), "EmailCodeStore")
}

// TestDepsValidateZeroOptional 钉住判据范围：只填必选时必须通过，且不得 panic。
//
// 这条是 typed-nil 判据的实现护栏：Deps 里 4 个字段是非 nilable kind
// （Retention 结构体、两个 int、一个 Duration）。如果判据写成
// `reflect.ValueOf(v).IsNil()` 一把梭，这里会 panic（IsNil on struct Value）——
// 装配期 panic 等于服务起不来。
func TestDepsValidateZeroOptional(t *testing.T) {
	d := Deps{Store: newFakeStore(), EmailCodeStore: testEmailCodes}
	require.NoError(t, d.Validate())
}

// TestDepsValidateTypedNil 是 2026-08-10 事故的回归：nil *ViewLoader 装进接口
// 后 `!= nil` 判真，写到 nil receiver panic。Validate 要在装配期抓住它并指名。
func TestDepsValidateTypedNil(t *testing.T) {
	d := Deps{
		Store:              newFakeStore(),
		EmailCodeStore:     testEmailCodes,
		SupplierViewReload: (*nilViewLoader)(nil),
	}
	err := d.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "SupplierViewReload", "错误必须指名字段，否则装配期无法定位")
	require.Contains(t, err.Error(), "typed-nil")
}

// nilStore 内嵌 Store 接口：nil 指针也能满足接口，用于构造最宽必选面的装箱
// 陷阱（方法被调到才炸，正是这类 bug 难查的原因）。
type nilStore struct{ Store }

// TestDepsValidateTypedNilStore 必选面同样会装箱：Store 装了 nil 具体值之后
// `Store == nil` 为假，必选判据放行，直到第一次查询才炸。Store 是全仓最宽的
// 依赖面，漏检它等于 typed-nil 判据只覆盖了一半。
func TestDepsValidateTypedNilStore(t *testing.T) {
	d := Deps{Store: (*nilStore)(nil), EmailCodeStore: testEmailCodes}
	err := d.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "Store")
	require.Contains(t, err.Error(), "typed-nil")
}

// TestDepsValidateValueImplementation 反例：值型实现（NopInvalidator{}）不是
// typed-nil。判据若把 Interface kind 之外的都交回去，这里必须过。
func TestDepsValidateValueImplementation(t *testing.T) {
	d := Deps{Store: newFakeStore(), EmailCodeStore: testEmailCodes, Invalidate: NopInvalidator{}}
	require.NoError(t, d.Validate())
}

// TestDepsFeatures 钉住「这次开了什么」的可观测出口：只列已装配的可选面，
// 稳定排序，零值 Deps 返回空而不是 nil 之外的怪东西。
func TestDepsFeatures(t *testing.T) {
	require.Empty(t, Deps{}.Features())

	d := Deps{
		Store:          newFakeStore(),
		EmailCodeStore: testEmailCodes,
		Invalidate:     NopInvalidator{},
		Publisher:      nopPublisher{},
		CompileNotify:  compileTriggerFunc(func() {}),
	}
	got := d.Features()
	require.Equal(t, []string{"compile_trigger", "invalidate", "publisher"}, got,
		"Features 必须排序稳定（排序键就是输出顺序）")
}

// nopPublisher 只用于 Features 的开关判定。
type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, notify.Change) error { return nil }
