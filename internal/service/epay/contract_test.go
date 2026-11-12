// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package epay

// contract_test.go 是 P51 的 **epay 深度判据**（spec v2 §13 P51 / §18-6 / N47）。
//
// 分工写死，两条互不冒充：
//
//   - 协议无关的部分（字段名非空去重、空对象被拒、未知键被拒、SecretNames 派生）
//     在 `internal/paymentregistry/contract_test.go` 里表驱动跑**全部注册协议**
//     ——新增协议自动被覆盖，不需要再写一条 P。
//   - 本文件只做 epay 的深度判据：用**真实 RSA PEM** 拼出全部 4 个字段的合法
//     config，逐个删掉一个必需键并断言错误**点名**该键。这是 r6 那个 bug（协议加了
//     字段、schema 漏了它 ⇒ 表单不出框 ⇒ 严格 JSON 下必然 400 "requires public_key"）
//     在 Go 侧唯一的保护，故键集合必须与 `ConfigSchema()` **双向对齐**（M37 的杀手）。

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
)

// epayConfigKeys ParseConfig 的**必需键**全集（epay 专项可以点名：这就是本协议
// 的判据本身）。刻意写成显式常量而不是从 ConfigSchema() 派生——派生会让 M37
// （schema 漏一个键）在"要删哪些键"这一步就被悄悄跳过，用例于是永远绿。
var epayConfigKeys = []string{
	ConfigFieldBaseURL,
	ConfigFieldPID,
	ConfigFieldPrivateKey,
	ConfigFieldPublicKey,
}

// generatedPEM 现场生成的一对 2048-bit 商户密钥（PKCS#8 私钥 / PKIX 公钥）。
//
// 为什么不用固定 PEM：P51 要证明"看起来合法的 PEM 真的能被 ParseConfig 解析"，
// 一次性常量会把判据悄悄退化成"解析器认识这段字符串"。
func generatedPEM(t *testing.T) (privatePEM, publicPEM string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

// legalEpayConfig 全部 4 个字段都合法的 config（值可分辨，便于断言 MerchantRef）。
func legalEpayConfig(t *testing.T) map[string]string {
	t.Helper()
	privatePEM, publicPEM := generatedPEM(t)
	return map[string]string{
		ConfigFieldBaseURL:    "https://pay.example.com",
		ConfigFieldPID:        "1001",
		ConfigFieldPrivateKey: privatePEM,
		ConfigFieldPublicKey:  publicPEM,
	}
}

// TestParseConfigAcceptsEverySchemaField P51「schema → ParseConfig」方向：schema 声明
// 的每个键都在一份合法 config 里，`ParseConfig` 必须**成功**并给出 Provider/MerchantRef。
//
// 反向（ParseConfig → schema）在下一个用例里用"逐个删键"承担。
func TestParseConfigAcceptsEverySchemaField(t *testing.T) {
	provider := New(nil)
	cfg := legalEpayConfig(t)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	parsed, err := provider.ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig(全部 4 个字段的合法 config) 必须成功，实际失败：%v", err)
	}
	if got := parsed.Provider(); got != domain.ProviderEpayV2 {
		t.Fatalf("cfg.Provider() = %q, want %q", got, domain.ProviderEpayV2)
	}
	if got := parsed.MerchantRef(); got != cfg[ConfigFieldPID] {
		t.Fatalf("cfg.MerchantRef() = %q, want %q（epay 的商户标识 = pid）", got, cfg[ConfigFieldPID])
	}

	// schema 的字段集合必须与这份"合法 config 的键集合"逐项相等：
	// 少一个 = 表单画不出框（严格 JSON 下必然 400）；多一个 = 用户照着表单填也 400。
	schemaNames := make([]string, 0, len(provider.ConfigSchema().Fields))
	for _, f := range provider.ConfigSchema().Fields {
		schemaNames = append(schemaNames, f.Name)
	}
	if len(schemaNames) != len(epayConfigKeys) {
		t.Fatalf("ConfigSchema() 声明了 %d 个字段 %v，ParseConfig 必需 %d 个键 %v",
			len(schemaNames), schemaNames, len(epayConfigKeys), epayConfigKeys)
	}
	declared := make(map[string]bool, len(schemaNames))
	for _, name := range schemaNames {
		declared[name] = true
	}
	for _, name := range epayConfigKeys {
		if !declared[name] {
			t.Fatalf("ConfigSchema() 漏了 %q：表单画不出这个输入框 → 严格 JSON 下必然 400", name)
		}
	}
}

// TestParseConfigRequiresEverySchemaField P51「ParseConfig → schema」方向：逐个删掉
// 一个必需键，`ParseConfig` 必须失败且错误信息**点名**该键（`requires <name>`）。
//
// 这是 M37 的判据：把 `public_key` 从 `ConfigSchema()` 删掉之后，下面的
// "schema ↔ 必需键"对齐断言立刻红——而 ParseConfig 仍然会拦"缺 public_key"，
// 于是漂移被夹在两条断言之间，不可能两边同时绿。
func TestParseConfigRequiresEverySchemaField(t *testing.T) {
	provider := New(nil)

	// 先对齐键集合：本用例要删的键 = ParseConfig 真正要求的键 = schema 声明的键。
	schemaNames := make([]string, 0, len(provider.ConfigSchema().Fields))
	for _, f := range provider.ConfigSchema().Fields {
		schemaNames = append(schemaNames, f.Name)
	}
	if len(schemaNames) != len(epayConfigKeys) {
		t.Fatalf("ConfigSchema() 与 ParseConfig 的键集合数量不一致：schema=%v 必需=%v（M37：schema 漏键）",
			schemaNames, epayConfigKeys)
	}
	for _, name := range epayConfigKeys {
		found := false
		for _, declared := range schemaNames {
			if declared == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ConfigSchema() 未声明必需键 %q（M37：schema 漏键 ⇒ 表单填不出来 ⇒ 必然 400）", name)
		}
	}

	for _, missing := range epayConfigKeys {
		t.Run("without_"+missing, func(t *testing.T) {
			cfg := legalEpayConfig(t)
			delete(cfg, missing)
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal config: %v", err)
			}
			_, parseErr := provider.ParseConfig(raw)
			if parseErr == nil {
				t.Fatalf("缺 %q 的 config 必须被拒绝（否则空字段的账户会进库，运行期才 503）", missing)
			}
			if !strings.Contains(parseErr.Error(), "requires "+missing) {
				t.Fatalf("错误信息必须点名缺失的键 %q（实际：%v）", missing, parseErr)
			}
		})
	}
}

// TestConfigSchemaSecretIsOnlyPrivateKey P51④ 的 epay 实例：`private_key` 是**唯一**
// 秘密字段。
//
// 它同时钉住 §15-5 的协议自述：公私钥成对，但公钥可以明文（它是验签输入），
// 私钥绝不能进浏览器——把 public_key 也标 secret 会让运维看不到自己配的是哪一对。
func TestConfigSchemaSecretIsOnlyPrivateKey(t *testing.T) {
	secrets := New(nil).ConfigSchema().SecretNames()
	if len(secrets) != 1 || secrets[0] != ConfigFieldPrivateKey {
		t.Fatalf("ConfigSchema().SecretNames() = %v, want [%s]", secrets, ConfigFieldPrivateKey)
	}
}
