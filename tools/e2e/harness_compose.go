// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：docker compose 调用是 harness 基础设施（普通 test job
// 不执行任何 docker；只编译，供 e2e-tagged services/sweep 使用）。
//
// 专用栈纪律（spec §0.1/§3.3/§10）：
//   - 自包含 compose 文件（tools/e2e/testdata/dedicated-compose.yml），**不与**
//     deploy/test-compose.yml 叠加——override 的 ports 是追加合并，base 的
//     15432/16379 仍会发布 => 专用栈必须独立定义 db+redis；
//   - 唯一 project e2e-<uuid8> + 随机 host 端口（"127.0.0.1:0:5432"）；
//   - stop/start 用 docker compose stop/start（保留端口映射），只有 cleanup 用 down -v；
//   - 共享 base 永不被 mutate。
package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// composeProjectLabel compose v2 自动给容器/网络/卷打的 project label（兜底清理
	// 不依赖 compose 文件路径仍存在）。
	composeProjectLabel = "com.docker.compose.project"

	composePreflightTimeout = 30 * time.Second
	composeUpTimeout        = 5 * time.Minute
	composeCmdTimeout       = 2 * time.Minute
)

// composeRun 执行 docker compose（project/file 为空则省略对应参数）。
func composeRun(project, file string, args ...string) (string, error) {
	return dockerRun(composeArgs(project, file, args...)...)
}

// composeArgs 构造 docker compose 参数序列（抽成纯函数便于静态审查）。
func composeArgs(project, file string, args ...string) []string {
	out := []string{"compose"}
	if project != "" {
		out = append(out, "-p", project)
	}
	if file != "" {
		out = append(out, "-f", file)
	}
	return append(out, args...)
}

// dockerRun 执行 docker CLI，返回 combined output（失败时附带输出便于排障）。
func dockerRun(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), composeCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("docker %s: %w (%s)", strings.Join(args, " "), err, tailText(text, 2000))
	}
	return text, nil
}

// tailText 截取尾部文本（诊断日志不整段刷屏）。
func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// composePreflight 专用栈前置检查：docker compose v2+ 可用（P0 不写死具体版本号，
// 随机 host 端口支持以实测为准；不支持/未安装 => 明确 Fatal 而不是静默 skip）。
func composePreflight() error {
	ctx, cancel := context.WithTimeout(context.Background(), composePreflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose 不可用（e2e 专用栈需要 docker compose v2+）: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// composeUp 启动专用栈并等全部 healthcheck healthy（--wait）。
func composeUp(file, project string) error {
	out, err := composeRun(project, file, "up", "-d", "--wait", "--remove-orphans")
	if err != nil {
		return fmt.Errorf("compose up %s: %w (%s)", project, err, tailText(out, 4000))
	}
	return nil
}

// composeDown 销毁专用栈（含卷/网络；只用于 cleanup）。
func composeDown(file, project string) error {
	out, err := composeRun(project, file, "down", "-v", "--remove-orphans", "--timeout", "10")
	if err != nil {
		return fmt.Errorf("compose down %s: %w (%s)", project, err, tailText(out, 4000))
	}
	return nil
}

// composeStop 停单个服务（保留容器与端口映射 => start 后端口不变）。
func composeStop(file, project, service string) error {
	out, err := composeRun(project, file, "stop", "--timeout", "10", service)
	if err != nil {
		return fmt.Errorf("compose stop %s/%s: %w (%s)", project, service, err, tailText(out, 2000))
	}
	return nil
}

// composeStart 启动已 stop 的服务（端口映射保留）。
func composeStart(file, project, service string) error {
	out, err := composeRun(project, file, "start", service)
	if err != nil {
		return fmt.Errorf("compose start %s/%s: %w (%s)", project, service, err, tailText(out, 2000))
	}
	return nil
}

// composePort 发现服务随机 host 端口（输出形如 127.0.0.1:54321）。
func composePort(file, project, service string, containerPort int) (string, error) {
	out, err := composeRun(project, file, "port", service, strconv.Itoa(containerPort))
	if err != nil {
		return "", fmt.Errorf("compose port %s/%s: %w", project, service, err)
	}
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	// compose 可能回 "0.0.0.0:54321" 或 "[::]:54321"：取冒号后端口，host 固定 127.0.0.1。
	idx := strings.LastIndex(line, ":")
	if idx < 0 || idx+1 >= len(line) {
		return "", fmt.Errorf("compose port %s/%s 输出无法解析: %q", project, service, line)
	}
	port, err := strconv.Atoi(line[idx+1:])
	if err != nil || port <= 0 {
		return "", fmt.Errorf("compose port %s/%s 端口非法: %q", project, service, line)
	}
	return "127.0.0.1:" + strconv.Itoa(port), nil
}

// composeContainerID 服务容器 id（空 = 容器不存在/已停）。
func composeContainerID(file, project, service string) (string, error) {
	out, err := composeRun(project, file, "ps", "-a", "-q", service)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return "", nil
	}
	return strings.TrimSpace(lines[0]), nil
}

// composeServiceHealthy 服务 healthcheck 是否 healthy（无 healthcheck = 非 healthy）。
func composeServiceHealthy(file, project, service string) (bool, error) {
	id, err := composeContainerID(file, project, service)
	if err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), composeCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Health.Status}}", id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("docker inspect %s: %w (%s)", id, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "healthy", nil
}

// composeLogs 专用栈日志（失败诊断 / artifact）。
func composeLogs(file, project string) (string, error) {
	return composeRun(project, file, "logs", "--no-color", "--tail", "500")
}

// cleanupComposeProject 兜底清理专用栈：优先按 registry 的 compose 文件 down -v；
// 文件丢失/损坏/记录来自旧 run 时按 project label 直接 docker rm -f/network rm/volume rm
// （不依赖 compose 文件路径仍存在，spec §9.4）。
func cleanupComposeProject(rec *runRecord) error {
	if rec.ComposeProject == "" {
		return nil
	}
	var errs []string
	if rec.ComposeFile != "" {
		if _, statErr := os.Stat(rec.ComposeFile); statErr == nil {
			if err := composeDown(rec.ComposeFile, rec.ComposeProject); err == nil {
				return nil
			} else {
				errs = append(errs, err.Error())
			}
		}
	}
	if err := removeComposeProjectByLabels(rec.ComposeProject); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// removeComposeProjectByLabels 按 project label 强删残留容器/网络/卷。
func removeComposeProjectByLabels(project string) error {
	filter := "label=" + composeProjectLabel + "=" + project
	var errs []string
	// 容器（先停再删：--force 等价 rm -f）。
	if ids, err := dockerRun("ps", "-aq", "--filter", filter); err != nil {
		errs = append(errs, err.Error())
	} else if ids = strings.TrimSpace(ids); ids != "" {
		args := append([]string{"rm", "-f"}, strings.Fields(ids)...)
		if _, err := dockerRun(args...); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// 网络。
	if ids, err := dockerRun("network", "ls", "-q", "--filter", filter); err != nil {
		errs = append(errs, err.Error())
	} else if ids = strings.TrimSpace(ids); ids != "" {
		args := append([]string{"network", "rm"}, strings.Fields(ids)...)
		if _, err := dockerRun(args...); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// 卷（-v 语义的兜底：数据卷按 project label 删）。
	if ids, err := dockerRun("volume", "ls", "-q", "--filter", filter); err != nil {
		errs = append(errs, err.Error())
	} else if ids = strings.TrimSpace(ids); ids != "" {
		args := append([]string{"volume", "rm", "-f"}, strings.Fields(ids)...)
		if _, err := dockerRun(args...); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
