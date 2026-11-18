// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// 部署面是 M0b 的另一半契约，而它此前**零断言**：把 `-allow-contract` 塞进默认
// Job、删掉 hook 注解、backoffLimit 改 1、readiness 探针改回 /healthz、把
// migrations.job.enabled 默认成 false——五条变异全绿。
//
// 为什么模板断言走「去缩进后的连续行块」而不是 YAML 解析器：模板里有
// `{{- if }}` 一类 Go template 动作，任何 YAML 解析器都会把它当成非法文档；
// 而「碰巧包含某句话」的半截断言又会被注释、其它字段或第二个 args 项骗过。
// 连续行块是契约的形状：token 序列必须逐行在场，且必须**挨着**。
//
// values.yaml 没有模板动作，直接结构化解析：`backoffLimit: 0` 这类值必须
// 「键在场且为 0」（指针区分「没写」与「写了 0」——没写时 Helm 渲染出空的
// backoffLimit，Job 直接非法，而 0 是"失败即停"的唯一正确值）。
func TestDeployMigrateJobAndProbes(t *testing.T) {
	// --- 迁移 Job：hook 语义 + 重试/超时 + 参数只到 `-migrate up` 为止 ---
	job := deployFile(t, "deploy/helm/templates/job-migrate.yaml")
	jobLines := trimmedLines(job)

	require.Equal(t, "{{- if .Values.migrations.job.enabled }}", jobLines[0],
		"整个 Job 必须由 migrations.job.enabled 包住：关了它就不该渲染出任何 Job")

	requireLineBlock(t, jobLines, []string{
		`"helm.sh/hook": pre-install,pre-upgrade`,
		`"helm.sh/hook-weight": "-5"`,
		`"helm.sh/hook-delete-policy": before-hook-creation`,
	}, "迁移必须是 pre-install,pre-upgrade hook（失败时 helm 在动 Deployment 之前就停），"+
		"并按 before-hook-creation 清理上一次的 Job")

	requireLineBlock(t, jobLines, []string{
		"backoffLimit: {{ .Values.migrations.job.backoffLimit }}",
		"activeDeadlineSeconds: {{ .Values.migrations.job.activeDeadlineSeconds }}",
	}, "重试与超时必须来自 values（迁移不是可重试任务：失败即停；挂住的 DDL 要有硬上限）")

	// args 必须**恰好**是：config 路径、-migrate、up，然后列表结束。
	// 这是「Job 永不跑 contract」的强形式：contract 只能追加在 up 之后，而 up
	// 之后紧跟着的必须是 env（任何多余参数都会让这条红）。注释里出现
	// `-allow-contract` 不算——它在解释为什么永远不带。
	argsAt := requireLineBlock(t, jobLines, []string{
		"args:",
		`- "-config"`,
		"{{- if .Values.config.inline }}",
		`- "/etc/c3api/config.toml"`,
		"{{- else }}",
		`- ""`,
		"{{- end }}",
		`- "-migrate"`,
		`- "up"`,
	}, "Job 的参数必须是 config + `-migrate up`")
	require.Equal(t, "env:", jobLines[argsAt+9],
		"`-migrate up` 之后必须立刻结束 args：多一个参数（例如 -allow-contract）就会把收缩带进默认发布路径")

	// --- values：默认值就是发布契约 ---
	var values struct {
		Migrations struct {
			Job struct {
				Enabled               *bool `yaml:"enabled"`
				BackoffLimit          *int  `yaml:"backoffLimit"`
				ActiveDeadlineSeconds *int  `yaml:"activeDeadlineSeconds"`
			} `yaml:"job"`
		} `yaml:"migrations"`
		App struct {
			Strategy struct {
				MaxUnavailable *int `yaml:"maxUnavailable"`
			} `yaml:"strategy"`
		} `yaml:"app"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(deployFile(t, "deploy/helm/values.yaml")), &values))

	require.NotNil(t, values.Migrations.Job.Enabled, "values.yaml 必须显式给出 migrations.job.enabled")
	require.True(t, *values.Migrations.Job.Enabled,
		"默认必须开启迁移 Job：默认关掉等于默认漏跑迁移（/readyz 503 是唯一提示）")
	require.NotNil(t, values.Migrations.Job.BackoffLimit, "values.yaml 必须显式给出 migrations.job.backoffLimit")
	require.Zero(t, *values.Migrations.Job.BackoffLimit,
		"backoffLimit 必须是 0：迁移不是可重试任务，失败就停在这里让人看日志")
	require.NotNil(t, values.Migrations.Job.ActiveDeadlineSeconds, "values.yaml 必须显式给出 activeDeadlineSeconds")
	require.Equal(t, 600, *values.Migrations.Job.ActiveDeadlineSeconds,
		"activeDeadlineSeconds 必须是 600：跑十分钟的迁移是在等锁，不是在干活")
	require.NotNil(t, values.App.Strategy.MaxUnavailable, "values.yaml 必须显式给出 app.strategy.maxUnavailable")
	require.Zero(t, *values.App.Strategy.MaxUnavailable,
		"滚动升级必须 maxUnavailable: 0：宁可多起一个 pod，也不要先杀掉还能服务的旧副本")

	// --- Deployment：readiness 走 schema 门，liveness/startup 只证明进程活着 ---
	appLines := trimmedLines(deployFile(t, "deploy/helm/templates/deployment-app.yaml"))
	requireLineBlock(t, appLines, []string{
		"readinessProbe:", "httpGet:", "path: /readyz",
	}, "readiness 必须探 /readyz：schema 不可服务时新 pod 不能接流量")
	requireLineBlock(t, appLines, []string{
		"livenessProbe:", "httpGet:", "path: /healthz",
	}, "liveness 必须留在 /healthz：schema 漂移不该让编排器反复重启一个活着的进程")
	requireLineBlock(t, appLines, []string{
		"startupProbe:", "httpGet:", "path: /healthz",
	}, "startup 必须留在 /healthz：启动期只要进程还在就该放行（schema 判定由 readiness 表达）")
	requireLineBlock(t, appLines, []string{
		"maxUnavailable: {{ .Values.app.strategy.maxUnavailable }}",
	}, "滚动策略必须来自 values（默认 0）")
}

// deployFile 读仓库内的部署文件（repoRoot 由 logguard_manifest_test.go 提供：
// 向上找 go.mod，不依赖包目录的相对层级）。
func deployFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	require.NoError(t, err, "部署文件必须存在：%s", rel)
	return string(data)
}

// trimmedLines 把文件折成「去首尾空白、去空行」的行序列。
//
// 去缩进是刻意的：模板里的缩进会随 if/with 层级漂移，而 token 序列才是契约
// （缩进错了 helm template 会红，那是另一层验证）。
func trimmedLines(src string) []string {
	raw := strings.Split(src, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// requireLineBlock 断言 block 作为**连续**子序列出现在 lines 里（返回起始下标）。
// 连续性是关键：同样的几行散落在文件不同位置不构成契约。
//
// 失败诊断只给上下文窗口，不打印整份 manifest——整份文件（上百行）会淹没真正
// 想看的差异，而 manifest 本身在仓库里随时可读。
func requireLineBlock(t *testing.T, lines, block []string, what string) int {
	t.Helper()
	require.NotEmpty(t, block)
	for start := 0; start+len(block) <= len(lines); start++ {
		matched := true
		for i, want := range block {
			if lines[start+i] != want {
				matched = false
				break
			}
		}
		if matched {
			return start
		}
	}
	where := "block[0]（" + block[0] + "）在文件里完全不存在"
	for i, line := range lines {
		if line != block[0] {
			continue
		}
		lo, hi := max(0, i-2), min(len(lines), i+len(block)+2)
		where = "block[0] 在文件里出现多次或不连续，首个候选附近是：\n  " + strings.Join(lines[lo:hi], "\n  ")
		break
	}
	require.FailNow(t, "manifest block missing",
		"%s\n期望连续出现：\n  %s\n%s",
		what, strings.Join(block, "\n  "), where)
	return -1
}
