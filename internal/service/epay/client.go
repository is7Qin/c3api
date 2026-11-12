// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package epay

// client.go 网关 HTTP 面（§5-3 查单）：POST {epay_base}/api/pay/query。
//
// 响应契约（§5-3 + §15-5「第三方文档以联调抓包为准」）：
//   - 扁平 JSON，至少含 code / status / trade_no / out_trade_no / money / type /
//     sign / sign_type；
//   - **响应须验签**（平台公钥；sign_type 同样必须在验签前硬校验 == RSA）；
//   - `code != 0` → 失败；仅 `status == 1` 进完成流程（其余状态只记录不迁移）。
//
// 字符串化：JSON 里数字/布尔等标量一律取其字面文本参与验签（网关按字符串拼
// 签名串），只有字符串会去引号并反转义——不引入 float 中转。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	// submitPath/queryPath 协议路径（§5-2/§5-3）。
	submitPath = "/api/pay/submit"
	queryPath  = "/api/pay/query"
	// maxQueryResponseBytes 查单响应上限（防网关返回超大 body）。
	maxQueryResponseBytes = 64 << 10
)

// QueryRequest 一次查单所需凭据（provider 从**账户 cfg** 填入：v2 里同一
// 进程可能同时查多个商户的订单，凭据绝不能来自全局设置）。
type QueryRequest struct {
	BaseURL       string
	PID           string
	OutTradeNo    string
	PrivateKeyPEM string
	PublicKeyPEM  string
}

// Client 网关 HTTP 面：只做协议（签名、请求、验签、字段解析），不读配置。
type Client struct {
	hc *http.Client
}

// NewClient 构造协议客户端；hc nil → 10s 超时的默认客户端（生产由 main 显式
// 传入同参数客户端，测试注入 httptest 客户端）。
func NewClient(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{hc: hc}
}

// Query 查单并归一为 domain.NotifyEvent。错误分类：网络/解码/验签/code!=0 →
// 普通错误（管理面 502 上游故障）；配置/公钥问题 → 包 domain.ErrConfigMissing。
func (c *Client) Query(ctx context.Context, req QueryRequest) (domain.NotifyEvent, error) {
	if req.BaseURL == "" {
		return domain.NotifyEvent{}, fmt.Errorf("%w: epay %s", domain.ErrConfigMissing, ConfigFieldBaseURL)
	}
	if req.PID == "" {
		return domain.NotifyEvent{}, fmt.Errorf("%w: epay %s", domain.ErrConfigMissing, ConfigFieldPID)
	}
	if req.PrivateKeyPEM == "" {
		return domain.NotifyEvent{}, fmt.Errorf("%w: epay %s", domain.ErrConfigMissing, ConfigFieldPrivateKey)
	}
	if req.PublicKeyPEM == "" {
		return domain.NotifyEvent{}, fmt.Errorf("%w: epay %s", domain.ErrConfigMissing, ConfigFieldPublicKey)
	}
	params := url.Values{}
	params.Set("pid", req.PID)
	params.Set("out_trade_no", req.OutTradeNo)
	params.Set("timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	sign, err := SignValues(params, req.PrivateKeyPEM)
	if err != nil {
		return domain.NotifyEvent{}, err
	}
	params.Set(signField, sign)
	params.Set(signTypeField, signTypeRSA)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(req.BaseURL, "/")+queryPath, strings.NewReader(params.Encode()))
	if err != nil {
		return domain.NotifyEvent{}, fmt.Errorf("epay: build query request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return domain.NotifyEvent{}, fmt.Errorf("epay: query: %w", err)
	}
	defer resp.Body.Close() // nolint:errcheck // 只读响应，关闭错误不改变结论
	if resp.StatusCode != http.StatusOK {
		return domain.NotifyEvent{}, fmt.Errorf("epay: query: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxQueryResponseBytes+1))
	if err != nil {
		return domain.NotifyEvent{}, fmt.Errorf("epay: read query response: %w", err)
	}
	if len(body) > maxQueryResponseBytes {
		return domain.NotifyEvent{}, errors.New("epay: query response too large")
	}
	return parseQueryResponse(body, req.PublicKeyPEM)
}

// parseQueryResponse 解析并验签查单响应（§5-3）。
func parseQueryResponse(body []byte, publicKeyPEM string) (domain.NotifyEvent, error) {
	raw, err := flattenJSON(body)
	if err != nil {
		return domain.NotifyEvent{}, fmt.Errorf("epay: decode query response: %w", err)
	}
	params := make(url.Values, len(raw))
	for k, v := range raw {
		params.Set(k, v)
	}
	// sign_type 不受签名保护：必须在验签前硬校验，且不得据它选择算法（F41）。
	if got := params.Get(signTypeField); got != signTypeRSA {
		return domain.NotifyEvent{}, fmt.Errorf("epay: query response sign_type %q is not %q", got, signTypeRSA)
	}
	if err := VerifyValues(params, publicKeyPEM); err != nil {
		return domain.NotifyEvent{}, err
	}
	code, err := strconv.ParseInt(params.Get("code"), 10, 64)
	if err != nil {
		return domain.NotifyEvent{}, fmt.Errorf("epay: query response has no integer code: %v", err)
	}
	if code != 0 {
		return domain.NotifyEvent{}, fmt.Errorf("epay: query refused: code=%d msg=%q", code, params.Get("msg"))
	}
	paidMinor, err := parseMoney(params.Get("money"))
	if err != nil {
		return domain.NotifyEvent{}, fmt.Errorf("epay: query response money: %w", err)
	}
	status := domain.NotifyStatusOther
	if params.Get("status") == "1" {
		status = domain.NotifyStatusPaid
	}
	return domain.NotifyEvent{
		OutTradeNo: params.Get("out_trade_no"),
		TradeNo:    params.Get("trade_no"),
		PaidMinor:  paidMinor,
		// PaidCurrency 留空：币种由渠道声明决定，provider 按响应 type 解析后回填
		// （响应不带 type 时由 service 用订单快照兜底）。
		Status:    status,
		Timestamp: params.Get("timestamp"),
		Raw:       raw,
	}, nil
}

// flattenJSON 把扁平 JSON 对象摊平成 map[string]string（标量取字面文本）。
func flattenJSON(body []byte) (map[string]string, error) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("response is not a JSON object")
	}
	out := make(map[string]string, len(fields))
	for k, raw := range fields {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out[k] = s
			continue
		}
		out[k] = string(raw) // 数字/布尔/null：取字面文本（网关按字符串签名）
	}
	return out, nil
}

// parseMoney 把网关金额字符串解析为最小单位整数（分），**全程整数**：
// `^\d+(\.\d{1,2})?$`；>2 位小数 / 非数字 / 负数 → 错误（不四舍五入，B34）。
// 溢出按除法判（先判 major > (MaxInt64-cents)/100，不做乘法）。
func parseMoney(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty money")
	}
	major, frac, hasDot := strings.Cut(s, ".")
	if major == "" || len(major) > 18 {
		return 0, fmt.Errorf("money %q is not a decimal amount", s)
	}
	for _, r := range major {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("money %q is not a decimal amount", s)
		}
	}
	if hasDot && (len(frac) == 0 || len(frac) > 2) {
		return 0, fmt.Errorf("money %q has more than 2 decimals", s)
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("money %q is not a decimal amount", s)
		}
	}
	dollars, err := strconv.ParseInt(major, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money %q is not a decimal amount", s)
	}
	cents := int64(0)
	if hasDot {
		padded := frac + strings.Repeat("0", 2-len(frac))
		cents, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("money %q is not a decimal amount", s)
		}
	}
	// 溢出判据用除法：dollars*100 不得越过 MaxInt64。
	const maxInt64 = int64(^uint64(0) >> 1)
	if dollars > (maxInt64-cents)/100 {
		return 0, fmt.Errorf("money %q overflows minor units", s)
	}
	return dollars*100 + cents, nil
}
