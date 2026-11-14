// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package epay 易支付 V2 协议实现（spec 支付网关 §5）：签名、验签、支付跳转 URL、
// 查单。协议细节**不外泄**：service 只见 domain.PaymentProvider。
package epay

// sign.go 签名规则（§5-1，含 F41/F42/F47 硬约束）：
//
//   - 待签串：全部非空参数、剔 sign/sign_type、ASCII 升序、k=v 用 & 连接。
//   - sign_type 不受签名保护 ⇒ 必须**在验签前**硬校验 == RSA，且不得据它选择
//     算法（防算法混淆）。本文件的 Sign/Verify 不做这个判定，由 provider 在
//     进入验签前硬校验（唯一判定点）。
//   - 参与待签串的参数值（sign/sign_type 除外）含 & 或 = → 拒绝（消掉签名串
//     重建歧义）。**必须排除 sign**：RSA-2048 签名 256 字节 → 标准 base64
//     长度 344、尾部恒为 `==`，不排除会把每条合法通知都拒掉（B39）。
//   - 签：商户私钥 RSA-PKCS1v15 + SHA-256 → base64；验：平台公钥。
//   - 私钥 PKCS8 → PKCS1；公钥 PKIX → PKCS1。
//   - **密钥材料三种粘贴形态都收**（见 decodeKeyMaterial）：标准 PEM、换行被吃掉的
//     PEM、裸 base64（PKCS#1/PKCS#8/PKIX DER）。彩虹易支付后台给的就是裸 base64，
//     只认 PEM 会把真实商户密钥全部拒掉（线上事故形状：`private key is not PEM`）。
//   - base64 四试：Std / RawStd / URL / RawURL（缺 padding 的网关不该被判死）。

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	// signTypeRSA 唯一受支持的签名算法（sign_type 的硬校验值）。
	signTypeRSA = "RSA"
	// signField/signTypeField 不参与待签串。
	signField     = "sign"
	signTypeField = "sign_type"
)

// paramValueRejectChars 参与待签串的参数值里不允许出现的字符（F47）。
const paramValueRejectChars = "&="

// canonicalString 待签串：剔 sign/sign_type、跳过空值、ASCII 升序、`k=v` 以 `&`
// 连接（§5-1）。空值不参与（网关惯例：未填字段等于不存在）。
func canonicalString(params url.Values) string {
	keys := make([]string, 0, len(params))
	for k, vs := range params {
		if k == signField || k == signTypeField {
			continue
		}
		if len(vs) == 0 || vs[0] == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params.Get(k))
	}
	return b.String()
}

// checkUniqueKeys 重复键 → 拒绝（§5-1：url.Values 任一 key 多值即 fail，签名串
// 重建在重复键下有二义）。
func checkUniqueKeys(params url.Values) error {
	for k, vs := range params {
		if len(vs) > 1 {
			return fmt.Errorf("epay: duplicate parameter %q", k)
		}
	}
	return nil
}

// checkParamValues 参与待签串的参数值（sign/sign_type 除外）含 `&`/`=` → 拒绝
// （F47/B39）。sign 本身必然含 `=`（base64 padding），排除它是硬要求。
func checkParamValues(params url.Values) error {
	for k, vs := range params {
		if k == signField || k == signTypeField {
			continue
		}
		for _, v := range vs {
			if strings.ContainsAny(v, paramValueRejectChars) {
				return fmt.Errorf("epay: parameter %q contains %q (signature ambiguity)", k, paramValueRejectChars)
			}
		}
	}
	return nil
}

// SignValues 计算待签串签名并返回标准 base64 的签名（Std，尾部带 padding）。
// params 需已含全部业务参数（不含 sign；sign_type 会被剔除）。
func SignValues(params url.Values, privateKeyPEM string) (string, error) {
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonicalString(params)))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("epay: sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifyValues 校验 params 的签名（配置/公钥问题 → 包 domain.ErrConfigMissing，
// 由调用方回 5xx；其余 → 普通错误，调用方回 200 fail）。
func VerifyValues(params url.Values, publicKeyPEM string) error {
	if err := checkUniqueKeys(params); err != nil {
		return err
	}
	if err := checkParamValues(params); err != nil {
		return err
	}
	sign := params.Get(signField)
	if sign == "" {
		return errors.New("epay: missing sign")
	}
	key, err := parseRSAPublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(canonicalString(params)))
	var lastErr error
	for _, cand := range signCandidates(sign) {
		raw, err := decodeSignature(cand)
		if err != nil {
			lastErr = err
			continue
		}
		// 先解码再 rsa.VerifyPKCS1v15（内部常量时间比较摘要）。
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], raw); err == nil {
			return nil
		}
		lastErr = errors.New("epay: signature mismatch")
	}
	return fmt.Errorf("epay: verify: %w", lastErr)
}

// signCandidates 取签名候选值（§5-1 / F60）：domain.PaymentProvider 只拿到
// url.Values，RawQuery 已在解析时丢弃；网关若未做 percent-encoding，base64 里的
// `+` 会被 url.ParseQuery 解成空格——因此额外尝试「空格还原为 `+`」。
// 正常 percent-encoded 的 `+` 解析后仍是 `+`，原值即命中。
func signCandidates(sign string) []string {
	out := []string{sign}
	if alt := strings.ReplaceAll(sign, " ", "+"); alt != sign {
		out = append(out, alt)
	}
	return out
}

// base64Variants 四种 base64 变体（缺 padding / URL-safe 的网关不该被判死）。
var base64Variants = []*base64.Encoding{
	base64.StdEncoding,
	base64.RawStdEncoding,
	base64.URLEncoding,
	base64.RawURLEncoding,
}

// decodeSignature 依次尝试四种 base64 变体解码签名。
func decodeSignature(sign string) ([]byte, error) {
	var firstErr error
	for _, enc := range base64Variants {
		raw, err := enc.DecodeString(sign)
		if err == nil {
			return raw, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("epay: signature is not base64 (std/rawstd/url/rawurl): %w", firstErr)
}

// decodeKeyMaterial 把"粘贴进来的密钥材料"统一成 DER 字节。**三种真实形态都要收**：
//
//  1. 标准 PEM（`-----BEGIN ...-----` 包裹、行宽 64）；
//  2. **换行被吃掉的 PEM**：单行 `<input>` / JSON 往返常见，marker 还在但 body 全在一行；
//  3. **裸 base64**：彩虹易支付后台给私钥/公钥的方式常常就是 PKCS#8 / PKIX DER 的
//     base64，**没有 PEM 头**。只认 PEM 会把真实商户密钥全部拒掉——这正是线上
//     `private key is not PEM` 的来历。
//
// 另外容忍：首尾空白、成对引号（从 shell/env 连引号一起粘）、字面量 `\n`（后端把
// 换行转义后存进 jsonb 的形态）、base64 体里的折行/空格（很多面板按 64 列折行）。
func decodeKeyMaterial(text string) ([]byte, error) {
	s := strings.TrimSpace(text)
	if s == "" {
		return nil, errors.New("empty key material")
	}
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	// 字面量转义（JSON 里 "\n" 没被还原）：只在**完全没有真实换行**时替换，
	// 否则会误伤 body 里恰好出现的反斜杠+字母组合（base64 字母表不含 `\`）。
	if !strings.ContainsAny(s, "\r\n") && strings.Contains(s, `\n`) {
		s = strings.ReplaceAll(s, `\r`, "\r") // `\r\n` 由这两条覆盖
		s = strings.ReplaceAll(s, `\n`, "\n")
	}
	// ① 标准 PEM（pem.Decode 自己会跳过前导空白/多余文本）。
	if block, _ := pem.Decode([]byte(s)); block != nil {
		return block.Bytes, nil
	}
	// ② marker 还在、换行没了：取 BEGIN/END 之间的文本当 base64 解。
	if body, ok := pemBodyWithoutNewlines(s); ok {
		if der, err := decodeBase64Lenient(body); err == nil {
			return der, nil
		}
	}
	// ③ 裸 base64。
	return decodeBase64Lenient(s)
}

// pemBodyWithoutNewlines 从"单行 PEM"里取出 base64 主体（`-----BEGIN X-----` 之后、
// `-----END X-----` 之前的文本）。marker 不完整时返回 ok=false，交给裸 base64 兜底。
func pemBodyWithoutNewlines(s string) (string, bool) {
	const beginTag = "-----BEGIN "
	i := strings.Index(s, beginTag)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(beginTag):]
	j := strings.Index(rest, "-----") // BEGIN 行的收尾
	if j < 0 {
		return "", false
	}
	body := rest[j+len("-----"):]
	if k := strings.Index(body, "-----END "); k >= 0 {
		body = body[:k]
	}
	return body, true
}

// decodeBase64Lenient 去掉空白后依次试四种 base64 变体（与签名解码同一组变体）。
func decodeBase64Lenient(s string) ([]byte, error) {
	// 空白一律剥掉（含空格）：单行 `<input>` 在部分平台会把粘贴进来的换行**换成
	// 空格**而不是删掉，PDF/Word 里复制的 PEM 也常带空格。这里的输入是密钥材料
	// （base64 字母表不含空格），剥掉是无损的；签名路径**不走本函数**——签名里的
	// 空格有语义（F60 的 `+` 还原），那边不能用这套宽松规则。
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, s)
	if clean == "" {
		return nil, errors.New("empty key material")
	}
	var firstErr error
	for _, enc := range base64Variants {
		raw, err := enc.DecodeString(clean)
		if err == nil {
			return raw, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, fmt.Errorf("not base64 (std/rawstd/url/rawurl): %w", firstErr)
}

// parseRSAPrivateKey 解析 RSA 私钥：PKCS1 直读，否则按 PKCS8 解（PKCS8 → PKCS1）。
// 配置缺失/格式错一律包 domain.ErrConfigMissing（§6：503 + 缺失键名，不回显私钥）。
// 材料形态（PEM / 单行 PEM / 裸 base64）由 decodeKeyMaterial 统一。
func parseRSAPrivateKey(text string) (*rsa.PrivateKey, error) {
	der, err := decodeKeyMaterial(text)
	if err != nil {
		return nil, fmt.Errorf("%w: private key must be PEM or bare base64 (PKCS#1/PKCS#8): %v",
			domain.ErrConfigMissing, err)
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: private key must be PEM or bare base64 (PKCS#1/PKCS#8): %v",
			domain.ErrConfigMissing, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: private key is not RSA", domain.ErrConfigMissing)
	}
	return key, nil
}

// parseRSAPublicKey 解析 RSA 公钥：PKCS1 直读，否则按 PKIX 解（PKIX → PKCS1）。
// 材料形态同上（易支付的平台公钥同样常给裸 base64）。
func parseRSAPublicKey(text string) (*rsa.PublicKey, error) {
	der, err := decodeKeyMaterial(text)
	if err != nil {
		return nil, fmt.Errorf("%w: public key must be PEM or bare base64 (PKCS#1/PKIX): %v",
			domain.ErrConfigMissing, err)
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("%w: public key must be PEM or bare base64 (PKCS#1/PKIX): %v",
			domain.ErrConfigMissing, err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: public key is not RSA", domain.ErrConfigMissing)
	}
	return key, nil
}
