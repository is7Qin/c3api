// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// e2eServices：基础设施所有权抽象（spec §4.1）。
//
//   - shared：只读共享 base PG/Redis（TEST_DATABASE_URL / C3API_REDIS_ADDR）；
//     隔离靠每 run 唯一库 + 唯一 Redis 逻辑库；**无权 mutate 服务** => Stop/Start 返回错误。
//   - dedicated：自包含专用 compose 栈（testdata/dedicated-compose.yml，唯一 project
//     e2e-<uuid8>，随机 host 端口）；stop/start 用 docker compose stop/start（保留端口），
//     cleanup 用 down -v；不要求 base env。
//
// 场景只见 run.services() 暴露的接口，不得直接碰 docker（boundary gate 守门）。
package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// e2eServices 基础设施生命周期接口（spec §4.1；stop/start 是 dedicated 独有语义）。
type e2eServices interface {
	// PGAdminDSN base（或专用栈）PostgreSQL 的连接串（harness 在其上 CREATE 每 run 唯一库）。
	PGAdminDSN() string
	// RedisAddr base（或专用栈）Redis 地址（harness 只把地址写进 config TOML，
	// 子进程不继承 env）。
	RedisAddr() string
	// StopService / StartService 仅 dedicated 实现；shared 必须返回错误。
	StopService(t *testing.T, name string) error
	StartService(t *testing.T, name string) error
	WaitHealthy(t *testing.T, name string)
	Logs(t *testing.T) string
	// Close 释放本模式持有的基础设施（shared=no-op）并返回失败；失败时 harness 保留
	// registry 记录，交给下一次 stale sweep 兜底重试（绝不静默泄漏资源且不留证据）。
	Close(t *testing.T) error
}

// serviceDB / serviceRedis 专用栈服务名（compose service 键；禁止在场景里出现）。
const (
	serviceDB    = "db"
	serviceRedis = "redis"
)

// sharedServices 只读共享 base（部署前置：docker compose -f deploy/test-compose.yml up -d）。
type sharedServices struct {
	adminDSN  string
	redisAddr string
}

const (
	// 本地开发缺省（与 deploy/test-compose.yml 对齐；CI 显式注入 env）。
	sharedDefaultAdminDSN  = "postgres://postgres:c3api@localhost:15432/postgres"
	sharedDefaultRedisAddr = "127.0.0.1:16379"
)

func newSharedServices(t *testing.T) *sharedServices {
	t.Helper()
	return &sharedServices{
		adminDSN:  envOr("TEST_DATABASE_URL", sharedDefaultAdminDSN),
		redisAddr: envOr("C3API_REDIS_ADDR", sharedDefaultRedisAddr),
	}
}

func (s *sharedServices) PGAdminDSN() string { return s.adminDSN }
func (s *sharedServices) RedisAddr() string  { return s.redisAddr }

// StopService shared 模式无权 mutate 共享 base：明确返回错误（spec §3.3）。
func (s *sharedServices) StopService(*testing.T, string) error {
	return fmt.Errorf("shared base 服务不可被测试停止（需要 stop/start 语义请用 e2eRunOptions{DedicatedServices:true}）")
}

func (s *sharedServices) StartService(*testing.T, string) error {
	return fmt.Errorf("shared base 服务不可被测试启动（需要 stop/start 语义请用 e2eRunOptions{DedicatedServices:true}）")
}

func (s *sharedServices) WaitHealthy(*testing.T, string) {
	// shared 无健康所有权：base 健康由部署前置/CI job 负责。
}

func (s *sharedServices) Logs(*testing.T) string { return "" }

func (s *sharedServices) Close(*testing.T) error { return nil }

// dedicatedServices 自包含专用栈（db + redis，随机 host 端口，唯一 project）。
type dedicatedServices struct {
	project     string
	composeFile string
	pgDSN       string
	redisAddr   string
}

// newDedicatedServices 构造专用栈描述（**不**启动）：registry 需要先把
// project/file/labels 写进去，之后才允许 up（AV-β：专用栈启动前 registry 已写）。
func newDedicatedServices(t *testing.T, uuid string) *dedicatedServices {
	t.Helper()
	root, err := repoRootDir()
	require.NoError(t, err, "定位仓库根目录")
	file := filepath.Join(root, "tools", "e2e", "testdata", "dedicated-compose.yml")
	_, err = os.Stat(file)
	require.NoError(t, err, "专用 compose 文件必须存在：%s", file)
	short := uuid
	if len(short) > 8 {
		short = short[:8]
	}
	return &dedicatedServices{project: "e2e-" + short, composeFile: file}
}

// containerLabels registry 记录的容器 label 维度（兜底清理按 project label 过滤）。
func (s *dedicatedServices) containerLabels() map[string]string {
	return map[string]string{composeProjectLabel: s.project}
}

// up 启动专用栈（preflight -> compose up --wait -> 端口发现）。
func (s *dedicatedServices) up(t *testing.T) error {
	t.Helper()
	if err := composePreflight(); err != nil {
		return err
	}
	if err := composeUp(s.composeFile, s.project); err != nil {
		return err
	}
	dbAddr, err := composePort(s.composeFile, s.project, serviceDB, 5432)
	if err != nil {
		return err
	}
	redisAddr, err := composePort(s.composeFile, s.project, serviceRedis, 6379)
	if err != nil {
		return err
	}
	s.pgDSN = "postgres://postgres:c3api@" + dbAddr + "/postgres?sslmode=disable"
	s.redisAddr = redisAddr
	return nil
}

func (s *dedicatedServices) PGAdminDSN() string { return s.pgDSN }
func (s *dedicatedServices) RedisAddr() string  { return s.redisAddr }

// validateServiceName 只允许专用栈内的服务名（陌生名字 = 编程错误）。
func (s *dedicatedServices) validateServiceName(name string) error {
	switch name {
	case serviceDB, serviceRedis:
		return nil
	default:
		return fmt.Errorf("未知专用栈服务 %q（合法：%s / %s）", name, serviceDB, serviceRedis)
	}
}

// StopService 停服务（保留容器/端口映射 => StartService 后端口不变）。
func (s *dedicatedServices) StopService(t *testing.T, name string) error {
	t.Helper()
	if err := s.validateServiceName(name); err != nil {
		return err
	}
	return composeStop(s.composeFile, s.project, name)
}

// StartService 启动已 stop 的服务并等 healthcheck healthy。
func (s *dedicatedServices) StartService(t *testing.T, name string) error {
	t.Helper()
	if err := s.validateServiceName(name); err != nil {
		return err
	}
	if err := composeStart(s.composeFile, s.project, name); err != nil {
		return err
	}
	s.WaitHealthy(t, name)
	return nil
}

// WaitHealthy 有界轮询 health=healthy（compose up --wait 之外的 start 场景）。
func (s *dedicatedServices) WaitHealthy(t *testing.T, name string) {
	t.Helper()
	if err := s.validateServiceName(name); err != nil {
		t.Fatalf("%v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		healthy, err := composeServiceHealthy(s.composeFile, s.project, name)
		if err == nil && healthy {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("专用栈服务 %s 未在 90s 内 healthy（last err=%v）", name, err)
		}
		<-tick.C
	}
}

// Logs 专用栈日志（失败诊断 / artifact；不含凭据以外的运行时数据）。
func (s *dedicatedServices) Logs(t *testing.T) string {
	t.Helper()
	logs, err := composeLogs(s.composeFile, s.project)
	if err != nil {
		return fmt.Sprintf("compose logs 失败: %v", err)
	}
	return logs
}

// Close 销毁专用栈（down -v；失败按 project label 兜底）；返回错误 => harness 保留
// registry 记录，下一次 stale sweep 按记录/or label 重试（AV-β 兜底面）。
func (s *dedicatedServices) Close(t *testing.T) error {
	t.Helper()
	if err := composeDown(s.composeFile, s.project); err != nil {
		t.Logf("warn: 专用栈 down -v 失败（尝试 label 兜底）：%v", err)
		if lerr := removeComposeProjectByLabels(s.project); lerr != nil {
			return fmt.Errorf("专用栈清理失败（compose down + label 兜底均失败）：%v; %w", err, lerr)
		}
	}
	return nil
}
