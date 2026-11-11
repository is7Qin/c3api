// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/sdkbridge"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type countingBody struct {
	io.Reader
	closes atomic.Int64
}

func (b *countingBody) Close() error {
	b.closes.Add(1)
	return nil
}

func testPayload() []byte {
	return []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
}

func readAllBody(rc io.ReadCloser) ([]byte, error) { return io.ReadAll(rc) }

func TestStreamBodyOnceSuccessClosesOnce(t *testing.T) {
	body := &countingBody{Reader: strings.NewReader("data: ok\n\n")}
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	status, got, err := sdkbridge.StreamBodyOnce(
		context.Background(), "pat", "acct", tr, testPayload(), readAllBody,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "data: ok\n\n", string(got))
	require.Equal(t, int64(1), body.closes.Load(), "200 body 必须恰好关闭一次")
}

func TestStreamBodyOnceReadErrorClosesOnce(t *testing.T) {
	body := &countingBody{Reader: strings.NewReader("partial")}
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})
	status, _, err := sdkbridge.StreamBodyOnce(
		context.Background(), "pat", "acct", tr, testPayload(),
		func(rc io.ReadCloser) ([]byte, error) {
			_, _ = io.Copy(io.Discard, rc)
			return nil, io.ErrUnexpectedEOF
		},
	)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Zero(t, status, "200 body 读失败按传输失败处理")
	require.Equal(t, int64(1), body.closes.Load(), "readBody 失败也必须恰好关闭一次")
}

func TestStreamBodyOnceNon200ReturnsStatusAndRaw(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &countingBody{Reader: strings.NewReader("raw-upstream")}
			tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
			})
			readCalled := false
			gotStatus, raw, err := sdkbridge.StreamBodyOnce(
				context.Background(), "pat", "acct", tr, testPayload(),
				func(io.ReadCloser) ([]byte, error) {
					readCalled = true
					return nil, nil
				},
			)
			require.NoError(t, err)
			require.Equal(t, status, gotStatus)
			require.False(t, readCalled, "非 200 由 SDK 读尽，不调用 200 reader")
			require.Equal(t, "raw-upstream", string(raw))
			require.Equal(t, int64(1), body.closes.Load(), "非 200 body 由 SDK 恰好关闭一次")
		})
	}
}

func TestStreamBodyOnceTransportErrors(t *testing.T) {
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})
	status, body, err := sdkbridge.StreamBodyOnce(
		context.Background(), "pat", "acct", base, testPayload(), readAllBody,
	)
	require.Error(t, err)
	require.Zero(t, status)
	require.Nil(t, body)

	// nil transport / nil reader 是装配错误，不 panic。
	status, body, err = sdkbridge.StreamBodyOnce(context.Background(), "pat", "acct", nil, testPayload(), readAllBody)
	require.Error(t, err)
	require.Zero(t, status)
	require.Nil(t, body)

	status, body, err = sdkbridge.StreamBodyOnce(context.Background(), "pat", "acct", base, testPayload(), nil)
	require.Error(t, err)
	require.Zero(t, status)
	require.Nil(t, body)
}

func TestStreamBodyOnceRevoked401(t *testing.T) {
	body := &countingBody{Reader: strings.NewReader(`{"error":{"code":"token_invalidated","message":"revoked"}}`)}
	tr := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: body}, nil
	})
	status, raw, err := sdkbridge.StreamBodyOnce(
		context.Background(), "pat-bad", "acct", tr, testPayload(), readAllBody,
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, string(raw), "token_invalidated")
	require.Equal(t, int64(1), body.closes.Load())
}

func TestStreamBodyOnceCanceledPrefersTransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	status, body, err := sdkbridge.StreamBodyOnce(ctx, "pat", "acct", tr, testPayload(), readAllBody)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, status)
	require.Nil(t, body)
}
