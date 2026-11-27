// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build windows

package e2e

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winStillActive GetExitCodeProcess 的 STILL_ACTIVE（x/sys 未导出该常量）。
const winStillActive = 259

// procAlive 平台 helper（Windows）：OpenProcess+GetExitCodeProcess 判活。返回
// (alive, certain)；certain=false 表示查询被拒/未知错误——sweep 对不确定一律不回收。
func procAlive(pid int) (alive bool, certain bool) {
	if pid <= 0 {
		return false, true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return false, true // 进程不存在
		}
		return true, false // ERROR_ACCESS_DENIED 等：存在但不可查 => 保守按活
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true, false
	}
	return code == winStillActive, true
}

// procCreationTime 进程创建时间（Windows FILETIME，opaque 仅用于同 host 比对）
// ——PID 复用安全的唯一依据（spec §10.4）。
func procCreationTime(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, false
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), true
}

// setProcAttrs CREATE_NEW_PROCESS_GROUP：让 GenerateConsoleCtrlEvent 可定向投递
// CTRL_BREAK_EVENT（Windows 优雅停机路径）。孤儿回收不依赖它——见 procJob。
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// gracefulTerminate 优雅停机：向子进程组投递 CTRL_BREAK_EVENT（Go 的
// Process.Signal 在 Windows 只支持 Kill；main 对 os.Interrupt 走同一优雅路径）。
func gracefulTerminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("process not started")
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
}

// forceKillTree 强杀单个登记子进程：先校验 creation-time（PID 复用安全），再
// taskkill /F /T /PID。**不依赖父进程 PID 推导子树**（spec §10.9）：调用方按
// registry 逐个 child 处理。
func forceKillTree(child childProcess) error {
	if child.PID <= 0 {
		return nil
	}
	if alive, certain := procAlive(child.PID); certain && !alive {
		return nil
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
	out, err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(child.PID)).CombinedOutput()
	if err != nil {
		if alive, certain := procAlive(child.PID); certain && !alive {
			return nil
		}
		return fmt.Errorf("taskkill pid %d: %w (%s)", child.PID, err, out)
	}
	return nil
}

// killProcessGroup Windows 无 PGID 概念：整棵子树回收走 Job Object + 逐 PID
// （spec §10.9），此函数无操作。
func killProcessGroup(children []childProcess) error { return nil }

// procJob Windows 具名 Job Object（KILL_ON_JOB_CLOSE）：测试进程被 kill -9 时最后
// 一个 Job handle 关闭，内核自动终止 Job 内全部子进程 => OS 层不存在孤儿
// （spec §10.1）。handle 不继承给子进程（CreateJobObject 默认 non-inheritable），
// 否则最后一个 handle 会随子进程存活而延后关闭，KILL_ON_JOB_CLOSE 失效。
type procJob struct {
	handle  windows.Handle
	jobName string
}

func newRunJob(name string) (*procJob, error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("job name %q: %w", name, err)
	}
	h, err := windows.CreateJobObject(nil, namePtr)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject %q: %w", name, err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject %q: %w", name, err)
	}
	return &procJob{handle: h, jobName: name}, nil
}

func (j *procJob) name() string { return j.jobName }

// assign 子进程 Start 成功后立即调用；失败由调用方 warn 并降级到 creation-time
// 校验 + 逐个 taskkill（spec §10.9：不能假设 Job 一定生效）。
func (j *procJob) assign(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(j.handle, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject pid %d: %w", pid, err)
	}
	return nil
}

// terminate 整 Job 强杀（run cleanup 兜底；sweep 侧 owner 已死时 Job 名已销毁，
// 只能走逐 PID，见 sweep 实现）。
func (j *procJob) terminate() error {
	if j.handle == 0 {
		return nil
	}
	return windows.TerminateJobObject(j.handle, 1)
}

func (j *procJob) close() error {
	if j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	return err
}
