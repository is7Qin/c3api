// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// privateOwner 是私有模块的属主前缀，也是授权的**唯一单位**。
//
// 为什么按属主而不是逐包：go.mod 里的私有依赖从一个（codex-sdk）长到两个
// （bps-sdk），而每一处凭证都逐包写死。加包的人不会记得去改三份 YAML 加一份
// Dockerfile，于是 bps-sdk 并进 main 的那天，CI 与镜像构建全线拉不到包。
// 属主前缀是稳定的，包列表是会长的——授权要挂在稳定的那一侧。
//
// 大小写敏感：module path 是小写 `github.com/is7qin/c3api`，两个 SDK 的路径却带
// 大写 Q。go 的模块路径匹配与 git 的 insteadOf 都是逐字节比较，所以这里写死真实
// 大小写，不能从 go.mod 的 module 行反推（推出来的是 `github.com/is7qin`，
// 逐字节不匹配，凭证一条也命中不了）。
const privateOwner = "github.com/is7Qin"

// privateAuthSites 冻结「哪些文件负责给构建流程开私有包的门」，值是文件里
// `GOPRIVATE=` 赋值点的个数。少一处 = 一条构建路径失去凭证（某个 job 的
// authorise 步骤被删、被并进别的步），这种事不该静默发生在 CI 里。
var privateAuthSites = map[string]int{
	".github/workflows/ci.yml":      2, // test 与 contract-gen 两个 job 各一处
	".github/workflows/release.yml": 1, // binaries 矩阵 job（镜像那条走 Dockerfile）
	"Dockerfile":                    1, // go mod download 之前
}

var (
	goprivateAssign = regexp.MustCompile(`GOPRIVATE=([A-Za-z0-9._,/!~-]+)`)
	insteadOfRule   = regexp.MustCompile(`url\."([^"]+)"\.insteadOf\s+"([^"]+)"`)
	buildkitSecret  = regexp.MustCompile(`type=secret,id=([A-Za-z0-9_]+)`)
	goModRequireLn  = regexp.MustCompile(`^\s*(github\.com/[^\s]+)\s+v[0-9]`)
)

// TestPrivateModuleAuthCoversEveryPrivateDependency 要求每一处授权都覆盖 go.mod 里
// **全部**属主私有依赖，而不是只覆盖写这段 YAML 时存在的那一个。
//
// 反过来说：把 `GOPRIVATE=github.com/is7Qin` 改成逐包列举（历史的写法），只要
// go.mod 里还有一个包没列进去，本门必红。
func TestPrivateModuleAuthCoversEveryPrivateDependency(t *testing.T) {
	root := repoRoot(t)

	mods := privateRequires(t, root)
	require.NotEmpty(t, mods,
		"go.mod 里没有 %s/ 名下的依赖——这道门会空转。私有 SDK 若真的都被移除了，请删掉本文件与四份授权", privateOwner)

	for rel, wantAssigns := range privateAuthSites {
		body := readRepoFile(t, root, rel)

		privates := goprivateValues(body)
		require.Len(t, privates, wantAssigns,
			"%s：`GOPRIVATE=` 赋值点 %d 处，冻结表要求 %d 处——某条构建路径的授权步骤被删/被并了？"+
				"若确实该改结构，请连同 privateAuthSites 一起改，并写明新的理由",
			rel, len(privates), wantAssigns)

		rules := insteadOfRewrites(body)
		require.NotEmpty(t, rules,
			"%s：没有任何 `url.….insteadOf` 重写——GOPRIVATE 只让 go 走直连，真正把 token 送出去的是这条 URL 重写；"+
				"只有一半（GOPRIVATE）等于匿名访问私有仓库，照样 404", rel)

		for _, mod := range mods {
			var covered bool
			for _, p := range privates {
				if coversModule(p, mod) {
					covered = true
					break
				}
			}
			if !covered {
				t.Errorf("%s：GOPRIVATE 未覆盖 %s（现有：%s）——该包会被送去公网代理，私有仓库返回 404",
					rel, mod, strings.Join(privates, ", "))
			}

			var rewritten bool
			for _, r := range rules {
				if coversModule(repoPathOf(r.target), mod) {
					require.True(t,
						strings.HasPrefix(r.base, "https://x-access-token:"),
						"%s：重写 %q → %q 没把 token 塞进 URL（应形如 https://x-access-token:<token>@<host>）",
						rel, r.target, r.base)
					rewritten = true
					break
				}
			}
			if !rewritten {
				t.Errorf("%s：insteadOf 未覆盖 %s——即便 GOPRIVATE 放行，go 仍会匿名 clone 私有仓库并 404", rel, mod)
			}
		}
	}
}

// TestDockerBuildWorkflowsPassEveryBuildkitSecret 钉住 Dockerfile 与 workflow 之间的
// secret 命名线：BuildKit 的 `required=false` 让「名字对不上」不报错，只在
// `go mod download` 处变成拉不到包。改 id、改 secret 名，都必须两边一起改。
func TestDockerBuildWorkflowsPassEveryBuildkitSecret(t *testing.T) {
	root := repoRoot(t)

	ids := uniqueSubmatches(buildkitSecret, readRepoFile(t, root, "Dockerfile"))
	require.NotEmpty(t, ids,
		"Dockerfile 里没有 `--mount=type=secret,id=…`——镜像构建将没有私有包凭证")

	workflows := dockerBuildWorkflows(t, root)
	require.NotEmpty(t, workflows,
		"没有任何 workflow 调用 docker/build-push-action——镜像构建若已迁走，请同步本门")

	for _, wf := range workflows {
		body := readRepoFile(t, root, wf)
		for _, id := range ids {
			require.Contains(t, body, id+"=${{ secrets.",
				"%s：没有把 BuildKit secret %q 传给 docker build。Dockerfile 里它是 required=false，"+
					"缺了不报错——只在 go mod download 处拉不到私有包", wf, id)
		}
	}
}

// privateRequires 列出 go.mod 里属主名下的全部依赖（含 indirect）。
func privateRequires(t *testing.T, root string) []string {
	t.Helper()
	var mods []string
	seen := map[string]bool{}
	for _, line := range strings.Split(readRepoFile(t, root, "go.mod"), "\n") {
		m := goModRequireLn.FindStringSubmatch(line)
		if m == nil || seen[m[1]] {
			continue
		}
		if !coversModule(privateOwner, m[1]) {
			continue
		}
		seen[m[1]] = true
		mods = append(mods, m[1])
	}
	return mods
}

// repoPathOf 把 insteadOf 的目标 URL 还原成模块路径：规则里写的是
// `https://github.com/is7Qin`，而 go.mod 里是 `github.com/is7Qin/bps-sdk`。
// git 对 from 侧做**纯前缀**匹配（不要求斜杠边界），所以 `…/is7Qin` 能改写
// `…/is7Qin/bps-sdk`，而 `…/is7Qin/codex-sdk` 改不了它。
func repoPathOf(rawurl string) string {
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(rawurl, scheme) {
			return strings.TrimPrefix(rawurl, scheme)
		}
	}
	return rawurl
}

// coversModule 判断前缀 prefix 是否覆盖模块路径 mod（自身或子路径）。
func coversModule(prefix, mod string) bool {
	return mod == prefix || strings.HasPrefix(mod, prefix+"/")
}

// goprivateValues 取出文件里每处 GOPRIVATE 的值（逗号分隔时逐个展开）。
func goprivateValues(body string) []string {
	var out []string
	for _, m := range goprivateAssign.FindAllStringSubmatch(body, -1) {
		for _, v := range strings.Split(m[1], ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

type insteadOf struct{ base, target string }

// insteadOfRewrites 取出文件里每条 `url."<base>".insteadOf "<target>"`。
func insteadOfRewrites(body string) []insteadOf {
	var out []insteadOf
	for _, m := range insteadOfRule.FindAllStringSubmatch(body, -1) {
		out = append(out, insteadOf{base: m[1], target: m[2]})
	}
	return out
}

// dockerBuildWorkflows 列出调用了 docker/build-push-action 的 workflow。
func dockerBuildWorkflows(t *testing.T, root string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	var out []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		if !strings.Contains(string(b), "docker/build-push-action") {
			continue
		}
		rel, err := filepath.Rel(root, p)
		require.NoError(t, err)
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func uniqueSubmatches(re *regexp.Regexp, body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

func readRepoFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoError(t, err, "读取 %s 失败——这道门依赖仓库布局，路径变了请同步", rel)
	return string(b)
}
