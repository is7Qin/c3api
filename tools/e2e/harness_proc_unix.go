// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build !windows

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// procAlive 平台 helper（Unix）：kill(pid, 0) 判活，Linux 额外把 zombie 视为已死。
// 返回 (alive, certain)：certain=false 表示状态不确定（EPERM/其它错误）——sweep
// 对「不确定」一律不回收（宁可泄漏不可误删）。
//
// kill(pid, 0) 对已退出但尚未被父进程 wait 的 zombie 仍返回成功；若只凭它判活，
// registry sweep / 自测会把「已 SIGKILL、等待 reap」的进程当成活进程（CI Linux 实测
// TestProcCreationTimeAndForceKill 10s 不收敛）。Linux 从 /proc/<pid>/stat 的 state
// 字段识别 Z/X；其它 Unix 没有 /proc，维持 kill(0) 语义（Darwin 的 procCreationTime
// 也不可用，sweep 本就保守跳过 PID/PGID 强杀）。
func procAlive(pid int) (alive bool, certain bool) {
	if pid <= 0 {
		return false, true
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		if runtime.GOOS == "linux" {
			state, ok, gone := linuxProcState(pid)
			if gone || (ok && (state == 'Z' || state == 'X')) {
				return false, true
			}
		}
		return true, true
	case errors.Is(err, syscall.ESRCH):
		return false, true
	case errors.Is(err, syscall.EPERM):
		return true, false
	default:
		return true, false
	}
}

// linuxProcState 读 /proc/<pid>/stat 的进程 state（字段 3）。ok=false 表示解析失败
// 或平台无 /proc；gone=true 表示进程在 kill(0) 与读取之间已消失（/proc 不存在）。
// comm 字段可含空格/括号，所以以最后一个 ')' 为界定位 state。
func linuxProcState(pid int) (state byte, ok bool, gone bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, true
		}
		return 0, false, false
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 || idx+1 >= len(data) {
		return 0, false, false
	}
	j := idx + 1
	for j < len(data) && (data[j] == ' ' || data[j] == '	') {
		j++
	}
	if j >= len(data) {
		return 0, false, false
	}
	return data[j], true, false
}

// procCreationTime 进程创建时间（opaque，仅用于同 host 内比对；registry 只处理
// hostname==本机的记录）。Linux 取 /proc/<pid>/stat 第 22 字段 starttime（boot 后
// tick 数）；其它 Unix（Darwin 等）本 P0 不提供 => (0,false)，sweep 因此保守
// 不按 PGID 强杀（宁泄漏不误杀）。
func procCreationTime(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// 字段 2 comm 可含空格/括号：以最后一个 ')' 为界，其后的字段从 state（字段 3）起。
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 > len(s) {
		return 0, false
	}
	fields := strings.Fields(s[idx+1:])
	// fields[0]=state(3) => starttime(22) 是 fields[22-3]。
	const startTimeIdx = 22 - 3
	if len(fields) <= startTimeIdx {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[startTimeIdx], 10, 64)
	if err != nil || v == 0 {
		return 0, false
	}
	return v, true
}

// setProcAttrs 每个被测子进程独立进程组：sweep 在测试进程被 kill -9 后可
// kill(-pgid, SIGKILL) 回收整棵子树（PID/进程组强杀前必须过 creation-time 校验，
// 见 killProcessGroup）。
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// gracefulTerminate 优雅停机：非 Windows 用 SIGTERM（main 的 NotifyContext 走同一
// 优雅路径）。
func gracefulTerminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}

// forceKillTree 强杀单个登记子进程：creation-time 与记录不一致 => 只报错不杀
// （PID 复用安全，spec §10.4）。
func forceKillTree(child childProcess) error {
	if child.PID <= 0 {
		return nil
	}
	if alive, certain := procAlive(child.PID); certain && !alive {
		return nil // 已死：无需强杀
	}
	if child.CreationTime != 0 {
		if ct, ok := procCreationTime(child.PID); ok {
			if ct != child.CreationTime {
				return errors.New("creation_time 不匹配，拒绝强杀（PID 可能被复用）")
			}
		} else if alive, certain := procAlive(child.PID); certain && !alive {
			return nil
		}
	}
	if err := syscall.Kill(child.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// killProcessGroup 按登记的进程组强杀整棵子树：组内至少一个成员的 creation-time
// 必须与记录一致才允许 kill(-pgid)（spec §9.3/§10.4；绝不 kill 自身或未校验的组）。
func killProcessGroup(children []childProcess) error {
	var pgid int
	for _, c := range children {
		if c.PGID > 0 {
			pgid = c.PGID
			break
		}
	}
	if pgid <= 0 {
		return nil
	}
	verified := false
	for _, c := range children {
		if c.PGID != pgid || c.PID <= 0 {
			continue
		}
		if c.CreationTime == 0 {
			continue
		}
		if ct, ok := procCreationTime(c.PID); ok && ct == c.CreationTime {
			verified = true
			break
		}
	}
	if !verified {
		// creation-time 无法校验（进程已全死 / 平台不支持）：不使用 kill(-pgid)；
		// 逐个 PID 的强杀仍由 forceKillTree 负责。
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// procJob Unix 侧无语义（孤儿回收靠进程组）；保留同名类型让跨平台 harness 代码
// 无需分叉。Windows 侧实现见 harness_proc_windows.go（Job Object + KILL_ON_JOB_CLOSE）。
type procJob struct{ jobName string }

func newRunJob(name string) (*procJob, error) { return &procJob{jobName: name}, nil }

func (j *procJob) name() string { return j.jobName }

func (j *procJob) assign(pid int) error { return nil }

func (j *procJob) terminate() error { return nil }

func (j *procJob) close() error { return nil }
