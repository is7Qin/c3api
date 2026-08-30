// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package snapshot 统一网关各内存快照的生命周期：启动就绪（ReloadAll 全量首刷）
// + NOTIFY 事件分发（按 scope 精确重载）+ 状态可观测（Status）。
//
// 边界（用户拍板 2026-08-11）：
//   - 注册表只做事件驱动（启动 + NOTIFY scope 分发）与状态追踪，不接管各模块
//     周期 ticker（scheduler syncLoop / balances ticker / auth-sync / price cron
//     均留模块自管——避免双 reload 竞争）；
//   - 不缓存/存储快照数据：数据仍在各模块（Auth RWMutex / scheduler store /
//     Balances atomic.Pointer / rulesMu / service pricing snapshot），注册表仅
//     持名称/scope/状态元数据；
//   - 不进入请求热路径：注册表所有锁只在启动/NOTIFY/管理面低频路径，快照读
//     取照旧模块内原子读，Reload 成本与各模块既有 reload 相同（零新增查询）。
//
// scope 语义（脏标记）：Snapshot 注册时声明关心的变更 scope；NOTIFY 变更按
// 类型映射 scope 后 Reload(ctx, scopes...) 只重载命中 scope 的快照（未命中
// 不动——变更标记对应快照集合，触发时只重载这些）。当前接线：settings 变更
// → ScopeSettings（auth gate N 预算即时重算，#36 缺口）。
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Scope 变更 scope 标识（脏标记键）。scope 字符串由装配侧按 NOTIFY 变更类型
// 映射；注册表只按字符串分发，不感知具体类型（保持最小接口）。
type Scope = string

// ScopeSettings settings 快照变更 scope：NOTIFY Change.Settings → 声明本 scope
// 的快照精确重载（当前接线 = auth：gate 预算按新 N 即时重算，补 #36 "NOTIFY
// 不触发 Auth.Reload" 缺口）。settings 自身快照（svc.ReloadSettings）不走注册
// 表，保持既有 ReloadSettings 行为。
const ScopeSettings Scope = "settings"

// Snapshot 最小快照契约（各模块已有 Reload 的包装，不侵入模块内部结构）：
// Name 唯一标识；Scopes 声明的变更 scope（nil = 纯启动/状态快照，不响应任何
// scope 分发）；Reload 全量刷新（须响应 ctx 取消；错误原样返回由注册表收集）。
type Snapshot interface {
	Name() string
	Scopes() []string
	Reload(ctx context.Context) error
}

// Status 单个快照的可观测状态（Status() 快照拷贝，供日志/管理面展示）。
type Status struct {
	Name       string
	Scopes     []string
	LastReload time.Time // 零值 = 尚未触发过
	LastError  error     // 最近一次 reload 错误（nil = 成功）
}

// ErrReloadPanic 快照 reload panic 的固定分类哨兵：Status.LastError 与返回
// map 错误共享的唯一类别（errors.Is 判定）。不携带 panic value 与 stack。
var ErrReloadPanic = errors.New("snapshot reload panicked")

const (
	// panicStackBufSize runtime.Stack 固定捕获 buffer（禁 debug.Stack 无界
	// 捕获后再截断——capture 即有界）。
	panicStackBufSize = 64 * 1024
	// panicStackMaxBytes sanitizer 最终字节上限（spec §4.2 固定顺序的收尾）。
	panicStackMaxBytes = 64 * 1024
)

// panicError Registry recovery 生成的私有 panic 诊断错误（spec §4.2）：只经
// Is(ErrReloadPanic) 暴露分类、只经 PanicStackForLog 提供脱敏 stack；不保存
// raw panic value、不支持 Unwrap（防 panic value 沿 error 链逃逸）。
type panicError struct {
	snapshot string
	stack    string // 已脱敏；空 = stack 一次性预算已消耗，本次未 capture
}

func (e *panicError) Error() string { return "snapshot reload panicked" }

func (e *panicError) Is(target error) bool { return target == ErrReloadPanic }

// PanicStackForLog 供 cmd/server operator 日志 helper 读取脱敏 stack（内部
// errors.As，调用方不依赖私有 panicError）：仅限日志用途——禁止进入 Status、
// HTTP、持久化、指标或通用 error 序列化。ok=false = 非 panic 错误或该次
// panic 的 stack 预算已消耗。
func PanicStackForLog(err error) (string, bool) {
	var pe *panicError
	if !errors.As(err, &pe) || pe.stack == "" {
		return "", false
	}
	return pe.stack, true
}

// sanitizePanicStack 规约已捕获 stack（spec §4.2 固定顺序）：按 frame 将文件
// 路径替换为 basename（/ 与 \ 同支持）→ 除 \n/\t 外控制字符替换 ?（字节级，
// UTF-8 连续字节 ≥0x80 不受影响）→ 按字节截断到 panicStackMaxBytes。
func sanitizePanicStack(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if j := max(strings.LastIndexByte(ln, '/'), strings.LastIndexByte(ln, '\\')); j >= 0 {
			ln = ln[j+1:]
		}
		lines[i] = ln
	}
	out := strings.Join(lines, "\n")
	b := []byte(out)
	for i, c := range b {
		if c == '\n' || c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			b[i] = '?'
		}
	}
	out = string(b)
	if len(out) > panicStackMaxBytes {
		out = out[:panicStackMaxBytes]
	}
	return out
}

// Registry 快照注册表：名称 → 快照 + scope → 快照名 的元数据索引（O(1) scope
// 分发查找——NOTIFY 低频路径）。触发（ReloadAll/Reload）内部串行执行（execMu，
// 事件不重叠；快照内部并发安全是模块既有保证，此处是注册表层面的事件串行），
// 单次触发内各快照并行 reload + 错误独立收集。注册与触发并发安全。
type Registry struct {
	mu      sync.RWMutex        // 保护元数据（order/byName/byScope/status/panicStackEmitted）
	order   []string            // 注册顺序（Status/错误收集确定性输出）
	byName  map[string]Snapshot // 名称 → 快照
	byScope map[string][]string // scope → 快照名（注册顺序；去重）
	status  map[string]*Status  // 名称 → 状态
	// panicStackEmitted per-name 一次性 stack 诊断预算（进程生命周期，spec
	// §5.3）：该快照是否已 capture 过一次完整 stack。Register 初始化；普通
	// success/error 不消耗不重置。无时钟、无配置项。
	panicStackEmitted map[string]bool
	// execMu 触发执行互斥：ReloadAll/Reload 串行。非重入——快照 Reload 内再触
	// 注册表（ReloadAll/Reload）即死锁（sync.Mutex 不可重入），快照必须自持
	// 状态，不得在 Reload 中回调注册表触发。
	execMu sync.Mutex
}

// New 构造空注册表。
func New() *Registry {
	return &Registry{
		byName:            make(map[string]Snapshot),
		byScope:           make(map[string][]string),
		status:            make(map[string]*Status),
		panicStackEmitted: make(map[string]bool),
	}
}

// Register 注册快照（重复名 → 错误；名称为空 → 错误）。注册与触发并发安全：
// 正在执行的触发已快照集合不受影响（本次注册的命中判定从下次触发起生效）。
func (r *Registry) Register(s Snapshot) error {
	if s == nil {
		return fmt.Errorf("snapshot: nil registration")
	}
	name := s.Name()
	if name == "" {
		return fmt.Errorf("snapshot: empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("snapshot: duplicate name %q", name)
	}
	scopes := slices.Clone(s.Scopes())
	r.byName[name] = s
	r.order = append(r.order, name)
	// scope 去重（byScope 注释承诺"去重"）：同一快照声明重复 scope 只入索引
	// 一次。Status.Scopes 同源去重（Status 展示即索引语义）。
	uniq := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		if sc == "" {
			continue
		}
		if _, dup := seen[sc]; dup {
			continue
		}
		seen[sc] = struct{}{}
		uniq = append(uniq, sc)
	}
	for _, sc := range uniq {
		r.byScope[sc] = append(r.byScope[sc], name)
	}
	r.status[name] = &Status{Name: name, Scopes: uniq}
	r.panicStackEmitted[name] = false
	return nil
}

// claimPanicStackBudget 消耗 per-name 一次性 stack 预算（r.mu 下 check-and-set，
// spec §5.3）：true = 本次允许 capture（预算刚消耗）；false = 已消耗，不再
// capture。判断先于 runtime.Stack——被抑制的 panic 不分配 stack。不同触发已
// 由 execMu 串行，r.mu 只防 Register/status 并发访问。
func (r *Registry) claimPanicStackBudget(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panicStackEmitted[name] {
		return false
	}
	r.panicStackEmitted[name] = true
	return true
}

// ReloadAll 全量并行 reload 所有已注册快照（启动就绪专用）：各快照错误独立
// 收集返回（map：快照名 → 错误，成功者不出现）；返回空 = 全部成功。单次触发
// 内并行执行，不同触发之间串行（execMu）。
func (r *Registry) ReloadAll(ctx context.Context) map[string]error {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	r.mu.RLock()
	names := slices.Clone(r.order)
	r.mu.RUnlock()
	return r.run(ctx, names)
}

// Reload 按 scope 精确重载（NOTIFY 分发）：只 reload 声明了任一给定 scope 的
// 快照（同快照多 scope 命中只执行一次）；空 scopes / 未命中 → no-op 返回空。
// 错误独立收集返回（同 ReloadAll）。O(命中 scope 的快照数) 查找。空 scopes
// 前置 return（不取 execMu——零状态读取，与触发执行互斥解耦，评审 P3-C）。
func (r *Registry) Reload(ctx context.Context, scopes ...string) map[string]error {
	if len(scopes) == 0 {
		return nil // 空 scopes 前置 return：零状态读取，不取 execMu（评审 P3-C）
	}
	r.execMu.Lock()
	defer r.execMu.Unlock()
	r.mu.RLock()
	names := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		for _, n := range r.byScope[sc] {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			names = append(names, n)
		}
	}
	r.mu.RUnlock()
	return r.run(ctx, names)
}

// reloadPair 注册名与快照成对快照（run 局部）：reload 后不再调用 s.Name()——
// 归因/记录/map 写入只用注册表名称（Name 可能 panic 或有副作用）。
type reloadPair struct {
	name string
	snap Snapshot
}

// reloadOnce 只执行 s.Reload(ctx) 并在同 goroutine 内恢复 panic（recovery
// 边界仅覆盖 Reload 调用本身——Registry 的 record/map/WaitGroup 簿记在调用方
// goroutine，不在此防护内，编程错误按原样暴露）。panicked=true 时 stack 为
// 一次性预算允许下的固定 buffer 捕获（可能为空 = 预算已消耗，未 capture）；
// 正常返回时普通 error 原样透传，不视作 Registry panic。recovered value 永不
// 格式化/存储/记录/unwrap（Go 1.26 默认 panic(nil) 恢复为非 nil runtime panic
// 值，与普通 panic 同路隔离）。
func reloadOnce(ctx context.Context, r *Registry, name string, s Snapshot) (err error, panicked bool, stack string) {
	defer func() {
		if recover() == nil {
			return // 正常返回（含普通 error）：未发生 panic
		}
		panicked = true
		if !r.claimPanicStackBudget(name) {
			return // stack 预算已消耗：本次不 capture（空 stack）
		}
		buf := make([]byte, panicStackBufSize)
		stack = sanitizePanicStack(string(buf[:runtime.Stack(buf, false)]))
	}()
	err = s.Reload(ctx)
	return err, false, ""
}

// run 并行执行 names 对应快照的 Reload，收集错误并记录状态。单快照 panic 被
// reloadOnce 隔离为该快照的 reload 失败：Status 记 ErrReloadPanic 固定摘要，
// 返回 map 写私有 *panicError（含一次性预算下的脱敏 stack）；兄弟照常执行。
func (r *Registry) run(ctx context.Context, names []string) map[string]error {
	if len(names) == 0 {
		return nil
	}
	r.mu.RLock()
	pairs := make([]reloadPair, 0, len(names))
	for _, n := range names {
		if s, ok := r.byName[n]; ok {
			pairs = append(pairs, reloadPair{name: n, snap: s})
		}
	}
	r.mu.RUnlock()
	errs := make(map[string]error)
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
	)
	for _, p := range pairs {
		wg.Add(1)
		go func(p reloadPair) {
			// wg.Done 在 recovery 闭包外：任何路径（含 panic 隔离）保证计数
			// 归零，run 不因 panic 永久等待。
			defer wg.Done()
			err, panicOccurred, stack := reloadOnce(ctx, r, p.name, p.snap)
			if panicOccurred {
				perr := &panicError{snapshot: p.name, stack: stack}
				r.record(p.name, ErrReloadPanic)
				errMu.Lock()
				errs[p.name] = perr
				errMu.Unlock()
				return
			}
			r.record(p.name, err)
			if err != nil {
				errMu.Lock()
				errs[p.name] = err
				errMu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return errs
}

// record 记录一次 reload 的结果（成功也更新 LastReload；LastError 置 nil 清
// 上次失败——Status 反映最近一次结果）。
func (r *Registry) record(name string, err error) {
	r.mu.Lock()
	if st, ok := r.status[name]; ok {
		st.LastReload = time.Now()
		st.LastError = err
	}
	r.mu.Unlock()
}

// Status 全部快照状态快照（注册顺序；值拷贝，调用方安全持有——Scopes 切片
// 深拷贝（slices.Clone），防调用方改写共享底层数组污染注册表状态）。
func (r *Registry) Status() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.order))
	for _, n := range r.order {
		if st, ok := r.status[n]; ok {
			cp := *st
			cp.Scopes = slices.Clone(st.Scopes)
			out = append(out, cp)
		}
	}
	return out
}
