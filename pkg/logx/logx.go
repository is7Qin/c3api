// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package logx 是 zap 的薄包装：业务代码只允许经本包取日志，禁止直接 import zap。
package logx

import (
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

type Logger struct{ l *zap.Logger }

func New(level, output string) (*Logger, error) {
	lv, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, err
	}
	if output == "" {
		output = "stdout"
	}
	cfg := zap.Config{
		Level:       zap.NewAtomicLevelAt(lv),
		Encoding:    "json",
		OutputPaths: []string{output},
		EncoderConfig: zapcore.EncoderConfig{
			MessageKey:   "msg",
			LevelKey:     "level",
			TimeKey:      "ts",
			CallerKey:    "caller",
			EncodeLevel:  zapcore.LowercaseLevelEncoder,
			EncodeTime:   zapcore.ISO8601TimeEncoder,
			EncodeCaller: zapcore.ShortCallerEncoder,
		},
	}
	z, err := cfg.Build(zap.AddCallerSkip(1))
	if err != nil {
		return nil, err
	}
	return &Logger{l: z}, nil
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

func (l *Logger) With(fields ...Field) *Logger { return &Logger{l: l.l.With(fields...)} }
func (l *Logger) Sync() error                  { return l.l.Sync() }
