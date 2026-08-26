// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/is7qin/c3api/internal/domain"
)

// --- 密码哈希（bcrypt DefaultCost(10)，与 sub2api 同参数） ---

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("s3cret-pass")
	require.NoError(t, err)
	require.True(t, VerifyPassword(hash, "s3cret-pass"))
	require.False(t, VerifyPassword(hash, "wrong"))
	require.False(t, VerifyPassword(hash, ""))
}

// 迁移兼容（评审 M-2）：sub2api 同参数（bcrypt DefaultCost=10）生成的 hash
// 直接可验证——用 bcrypt 库以 DefaultCost 独立生成，走本包 VerifyPassword。
func TestPasswordSub2apiHashCompatible(t *testing.T) {
	b, err := bcrypt.GenerateFromPassword([]byte("migrated-pass"), bcrypt.DefaultCost)
	require.NoError(t, err)
	require.True(t, VerifyPassword(string(b), "migrated-pass"), "sub2api 同参数 hash 可直接验证")
}

// 密码 ≤72 字节校验（bcrypt 截断限制）：超长拒绝而非静默截断（评审 M-2）。
func TestPasswordLenLimit(t *testing.T) {
	short := make([]byte, 72)
	for i := range short {
		short[i] = 'a'
	}
	require.NoError(t, ValidatePasswordLen(string(short)), "72 字节可接受")
	long := append(short, 'a')
	require.ErrorIs(t, ValidatePasswordLen(string(long)), ErrPasswordTooLong, "73 字节拒绝")
}

// --- JWT（HS256；TTL 24h；密钥强制在 config 层） ---

func TestJWTIssueVerify(t *testing.T) {
	iss := NewIssuer("test-secret")
	token, err := iss.Issue(42, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)

	claims, err := iss.Verify(token)
	require.NoError(t, err)
	require.Equal(t, int64(42), claims.UserID)
	require.Equal(t, "u@example.com", claims.Email)
	require.Equal(t, string(domain.RoleUser), claims.Role)
	require.True(t, claims.ExpiresAt.Time.After(time.Now()))
}

func TestJWTWrongSecretRejected(t *testing.T) {
	token, err := NewIssuer("secret-a").Issue(1, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	_, err = NewIssuer("secret-b").Verify(token)
	require.Error(t, err, "错误密钥必须拒绝")
	require.NotErrorIs(t, err, ErrTokenExpired, "非过期错误不得误分类为过期")
}

func TestJWTExpiredRejected(t *testing.T) {
	iss := &Issuer{secret: []byte("s"), ttl: -time.Minute} // 已过期
	token, err := iss.Issue(1, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	_, err = NewIssuer("s").Verify(token)
	require.Error(t, err, "过期 token 必须拒绝")
	// B2-2：过期分类归本包哨兵，同时保留 jwt/v5 原始链（errors.Is 双命中）
	require.ErrorIs(t, err, ErrTokenExpired, "过期错误必须命中本包哨兵")
	require.ErrorIs(t, err, jwt.ErrTokenExpired, "%w 保留 jwt/v5 原始链")
}

func TestJWTTamperedRejected(t *testing.T) {
	token, err := NewIssuer("s").Issue(1, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	_, err = NewIssuer("s").Verify(token + "x")
	require.Error(t, err, "篡改 token 必须拒绝")
}

// 算法替换防护：HS256 签发的 token 不能被 alg=none/其他算法重放。
func TestJWTAlgorithmConfusionRejected(t *testing.T) {
	claims := jwt.MapClaims{"user_id": float64(1), "exp": time.Now().Add(time.Hour).Unix()}
	noneToken, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	_, err = NewIssuer("s").Verify(noneToken)
	require.Error(t, err, "alg=none 必须拒绝")
}

// --- RequireJWT 中间件 ---

type fakeUserStatus struct{ snapshots map[int64]domain.UserSnapshot }

func (f fakeUserStatus) UserSnapshot(userID int64) (domain.UserSnapshot, bool) {
	s, ok := f.snapshots[userID]
	return s, ok
}

func doReq(t *testing.T, mw func(http.Handler) http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFrom(r.Context())
		if !ok {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(claims.Email))
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/user/keys", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRequireJWTValid(t *testing.T) {
	iss := NewIssuer("s")
	token, err := iss.Issue(7, "ok@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	mw := RequireJWT(iss, fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{7: {Status: domain.UserStatusActive}}})
	rec := doReq(t, mw, token)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok@example.com", rec.Body.String(), "claims 进入 context")
}

func TestRequireJWTRejects(t *testing.T) {
	iss := NewIssuer("s")
	token, err := iss.Issue(7, "ok@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)

	t.Run("no header", func(t *testing.T) {
		rec := doReq(t, RequireJWT(iss, fakeUserStatus{}), "")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		// 错误信封统一走 httpface.WriteErr（encoder 编码含尾换行；旧手搓
		// strconv.Quote 版无尾换行——行为变化，JSON 等价，断言锁定新语义）。
		require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		require.Equal(t, "{\"error\":\"unauthorized\"}\n", rec.Body.String())
	})
	t.Run("bad token", func(t *testing.T) {
		rec := doReq(t, RequireJWT(iss, fakeUserStatus{}), "garbage")
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	t.Run("expired", func(t *testing.T) {
		expired := &Issuer{secret: []byte("s"), ttl: -time.Minute}
		tok, _ := expired.Issue(7, "ok@example.com", string(domain.RoleUser), 0)
		rec := doReq(t, RequireJWT(iss, fakeUserStatus{}), tok)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	// 评审定夺②：禁用用户在快照中 → 立即拒绝（无需等 JWT 过期）
	t.Run("disabled user", func(t *testing.T) {
		mw := RequireJWT(iss, fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{7: {Status: domain.UserStatusDisabled}}})
		rec := doReq(t, mw, token)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	// B2-1 fail-closed：快照缺失（启动首刷失败/Reload 失败/NOTIFY 丢失）→
	// 401 拒绝，不放行（对照 /admin 面已 fail-closed）
	t.Run("snapshot missing", func(t *testing.T) {
		rec := doReq(t, RequireJWT(iss, fakeUserStatus{}), token)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "快照缺失必须拒绝而非放行")
	})
}

// token_version 撤销机制（spec 2026-08-25-jwt-password-revocation §5.1/5.2）：
// Issue 写入 ver → Verify 回读；快照版本与 claims.Ver 不匹配矩阵
// （0v1 = 改密后旧票拒 / 1v0 = 快照落后拒 / 1v1 = 改密后重登新票过）。
func TestJWTVerRoundTripAndRevocationMatrix(t *testing.T) {
	iss := NewIssuer("s")
	tokV0, err := iss.Issue(7, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	tokV1, err := iss.Issue(7, "u@example.com", string(domain.RoleUser), 1)
	require.NoError(t, err)

	c1, err := iss.Verify(tokV0)
	require.NoError(t, err)
	require.Zero(t, c1.Ver, "ver=0 落入 claims（存量平滑语义：DB 默认 0）")
	c2, err := iss.Verify(tokV1)
	require.NoError(t, err)
	require.Equal(t, int64(1), c2.Ver)

	for _, tc := range []struct {
		name        string
		token       string
		snapshotVer int64
		want        int
	}{
		{"claims ver0 vs snapshot ver1（改密 bump 后旧票）", tokV0, 1, http.StatusUnauthorized},
		{"claims ver1 vs snapshot ver0（快照未刷新，fail-closed 同向拒绝）", tokV1, 0, http.StatusUnauthorized},
		{"claims ver1 vs snapshot ver1（改密后重新登录新票）", tokV1, 1, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mw := RequireJWT(iss, fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{
				7: {Status: domain.UserStatusActive, TokenVersion: tc.snapshotVer},
			}})
			rec := doReq(t, mw, tc.token)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestRequireRole(t *testing.T) {
	iss := NewIssuer("s")
	token, _ := iss.Issue(7, "u@example.com", string(domain.RoleUser), 0)
	adminToken, _ := iss.Issue(8, "a@example.com", string(domain.RolePlatformAdmin), 0)
	// 快照含两用户（active）——fail-closed 下快照缺失 401，本测试聚焦
	// RequireRole 角色声明而非快照路径（快照缺失用例见 TestRequireJWTRejects）
	mw := func(next http.Handler) http.Handler {
		// 链序：RequireJWT（验证 + claims 入 ctx）→ RequireRole（角色声明）
		return RequireJWT(iss, fakeUserStatus{snapshots: map[int64]domain.UserSnapshot{
			7: {Status: domain.UserStatusActive},
			8: {Status: domain.UserStatusActive},
		}})(RequireRole(domain.RolePlatformAdmin)(next))
	}
	rec := doReq(t, mw, token)
	require.Equal(t, http.StatusForbidden, rec.Code, "user 角色访问 platform 端点 → 403")
	require.Equal(t, "{\"error\":\"forbidden\"}\n", rec.Body.String(), "403 信封 encoder 编码含尾换行")
	rec = doReq(t, mw, adminToken)
	require.Equal(t, http.StatusOK, rec.Code, "platform_admin 放行")
}
