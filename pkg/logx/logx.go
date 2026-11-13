// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package logx 是 zap 的薄包装：业务代码只允许经本包取日志，禁止直接 import zap。
package logx

import (
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Field 是 zap.Field 的别名，业务代码通过 logx 构造器创建字段。
type Field = zap.Field

func String(k, v string) Field                 { return zap.String(k, v) }
func Int(k string, v int) Field                { return zap.Int(k, v) }
func Int64(k string, v int64) Field            { return zap.Int64(k, v) }
func Duration(k string, v time.Duration) Field { return zap.Duration(k, v) }
func Bool(k string, v bool) Field              { return zap.Bool(k, v) }
func Error(err error) Field                    { return zap.Error(err) }
func Any(k string, v any) Field                { return zap.Any(k, v) }

// sinkCloser 持有输出 sink 的释放函数，供父 logger 与 With 派生的子 logger
// 共享：任一处 Close 都只真正释放一次（Windows 上不释放就无法删除文件）。
type sinkCloser struct {
	once sync.Once
	fn   func()
}

// release 释放 sink。nil 安全，可重复调用。
func (c *sinkCloser) release() {
	if c == nil {
		return
	}
	c.once.Do(c.fn)
}

type Logger struct {
	l      *zap.Logger
	closer *sinkCloser
}

// New 按 level 与 output 构建 JSON logger。output 为空等价于 "stdout"，
// 其余按 zap 的 sink 语义解释（文件路径/URL）。
//
// sink 由本包自己打开并持有，因此使用文件输出时调用方应当 Close：Windows
// 上文件句柄不关闭则进程内无法删除该文件（测试用临时目录会因此清理失败）。
func New(level, output string) (*Logger, error) {
	lv, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, err
	}
	if output == "" {
		output = "stdout"
	}
	ws, closeFn, err := zap.Open(output)
	if err != nil {
		return nil, err
	}
	encCfg := zapcore.EncoderConfig{
		MessageKey:   "msg",
		LevelKey:     "level",
		TimeKey:      "ts",
		CallerKey:    "caller",
		EncodeLevel:  zapcore.LowercaseLevelEncoder,
		EncodeTime:   zapcore.ISO8601TimeEncoder,
		EncodeCaller: zapcore.ShortCallerEncoder,
	}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), ws, lv)
	// AddCallerSkip(1) 抵掉本包的 Debug/Info/Warn/Error 包装层，caller 仍是业务调用点。
	z := zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1))
	return &Logger{l: z, closer: &sinkCloser{fn: closeFn}}, nil
}

func (l *Logger) Debug(msg string, fields ...Field) { l.l.Debug(msg, fields...) }
func (l *Logger) Info(msg string, fields ...Field)  { l.l.Info(msg, fields...) }
func (l *Logger) Warn(msg string, fields ...Field)  { l.l.Warn(msg, fields...) }
func (l *Logger) Error(msg string, fields ...Field) { l.l.Error(msg, fields...) }

// DebugEnabled level=debug 是否生效（accessLog 等 Debug 字段构造的 level 守卫
// ——zap 的字段编码短路在调用内，字段切片已在调用点物化逃逸；热路径先判再
// 构造，level 恒定无双读风险）。zapcore 不泄露（内部判定）。
func (l *Logger) DebugEnabled() bool {
	return l.l.Core().Enabled(zapcore.DebugLevel)
}

// With 派生带固定字段的子 logger；子 logger 与父共享 sink 所有权。
func (l *Logger) With(fields ...Field) *Logger {
	return &Logger{l: l.l.With(fields...), closer: l.closer}
}

func (l *Logger) Sync() error { return l.l.Sync() }

// Close 刷新并释放输出 sink（文件输出在 Windows 上不关闭则无法删除）。
// 可重复调用；nil logger 安全。Sync 的报错被忽略——sink 已关闭时不应再失败。
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	_ = l.l.Sync()
	l.closer.release()
	return nil
}
