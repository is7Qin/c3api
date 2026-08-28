package proxy

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/is7qin/c3api/pkg/sserelay"
	"github.com/stretchr/testify/require"
)

var modelRewriteSink []byte

func TestRewriteResponseModelJSONRewritesEveryExistingStringPath(t *testing.T) {
	body := []byte(`{"keep":9007199254740993,"message":{"model":"message-upstream"},"model":"root-upstream","response":{"model":"response-upstream"},"other":{"model":"untouched"}}`)

	got := rewriteResponseModelJSON(body, "client-model")

	require.Equal(t, `{"keep":9007199254740993,"message":{"model":"client-model"},"model":"client-model","response":{"model":"client-model"},"other":{"model":"untouched"}}`, string(got))
}

func TestRewriteResponseModelJSONLeavesUnsupportedInputsUntouched(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "missing paths", body: `{"response":{},"message":{},"other":{"model":"upstream"}}`},
		{name: "null", body: `{"model":null}`},
		{name: "number", body: `{"response":{"model":1}}`},
		{name: "object", body: `{"message":{"model":{"name":"upstream"}}}`},
		{name: "array", body: `{"model":[]}`},
		{name: "malformed", body: `{"model":"upstream"`},
		{name: "unrelated root array", body: `[{"model":"upstream"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)

			got := rewriteResponseModelJSON(body, "client-model")

			require.Equal(t, tc.body, string(got))
			require.Same(t, &body[0], &got[0])
		})
	}
}

func TestRewriteResponseModelJSONNoOpsReuseInputWithoutAllocations(t *testing.T) {
	cases := []struct {
		name, body, model string
	}{
		{name: "empty override", body: `{"model":"upstream"}`, model: ""},
		{name: "no match", body: `{"other":{"model":"upstream"}}`, model: "client-model"},
		{name: "already equal", body: `{"model":"client\/model","response":{"model":"\u0063lient/model"},"message":{"model":"client/model"}}`, model: "client/model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			got := rewriteResponseModelJSON(body, tc.model)
			require.Same(t, &body[0], &got[0])

			allocs := testing.AllocsPerRun(1000, func() {
				modelRewriteSink = rewriteResponseModelJSON(body, tc.model)
			})
			require.Zero(t, allocs)
		})
	}
}

func TestModelStringEqualsDecodesJSONEscapes(t *testing.T) {
	cases := []struct {
		raw, model string
		want       bool
	}{
		{raw: `"quote\"slash\\solidus\/"`, model: "quote\"slash\\solidus/", want: true},
		{raw: `"\b\f\n\r\t"`, model: "\b\f\n\r\t", want: true},
		{raw: `"\ud83d\ude80"`, model: "\U0001F680", want: true},
		{raw: `"\ud83d"`, model: "\uFFFD", want: true},
		{raw: `"client-model"`, model: "other-model", want: false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, modelStringEquals([]byte(tc.raw), tc.model), "raw=%s", tc.raw)
	}
}

func TestRewriteResponseModelSSEPreservesMetadataCRLFAndFrames(t *testing.T) {
	src := ": before\r\nevent: response.completed\r\nid: 42\r\nretry: 1000\r\ndata: {\"response\":{\"model\":\"upstream\"},\"keep\":9007199254740993}\r\n: after\r\n\r\ndata: [DONE]\r\n\r\n"
	want := ": before\r\nevent: response.completed\r\nid: 42\r\nretry: 1000\r\ndata: {\"response\":{\"model\":\"client-model\"},\"keep\":9007199254740993}\r\n: after\r\n\r\ndata: [DONE]\r\n\r\n"

	require.Equal(t, want, rewriteModelSSEStream(t, src, "client-model"))
}

func TestRewriteResponseModelSSENormalizesMultiDataOnlyWhenChanged(t *testing.T) {
	src := "event: message\ndata: {\"message\":\n: between\ndata: {\"model\":\"upstream\"},\"x\":1}\nid: 9\n\n"
	want := "event: message\ndata: {\"message\":{\"model\":\"client-model\"},\"x\":1}\n: between\nid: 9\n\n"

	require.Equal(t, want, rewriteModelSSEStream(t, src, "client-model"))
	require.Equal(t, src, rewriteModelSSEStream(t, src, ""))
}

func TestRewriteResponseModelSSELeavesOpaqueFramesUntouched(t *testing.T) {
	for _, src := range []string{
		"data: [DONE]\n\n",
		": comment\nid: opaque\nretry: 5\n\n",
		"data: {\"model\":null}\n\n",
		"data: {\"model\":\"upstream\"\n\n",
	} {
		require.Equal(t, src, rewriteModelSSEStream(t, src, "client-model"))
	}
}

func TestRewriteResponseModelSSENoOpsReuseInputWithoutAllocations(t *testing.T) {
	cases := []struct {
		name, raw, data, model string
	}{
		{name: "empty override", raw: "data: {\"model\":\"upstream\"}\n\n", data: `{"model":"upstream"}`, model: ""},
		{name: "no match", raw: "data: {\"other\":1}\n\n", data: `{"other":1}`, model: "client-model"},
		{name: "already equal", raw: "data: {\"model\":\"client-model\"}\n\n", data: `{"model":"client-model"}`, model: "client-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.raw)
			event := sserelay.Event{Raw: raw, Data: []byte(tc.data)}
			got := rewriteResponseModelSSE(event, tc.model)
			require.Same(t, &raw[0], &got[0])

			allocs := testing.AllocsPerRun(1000, func() {
				modelRewriteSink = rewriteResponseModelSSE(event, tc.model)
			})
			require.Zero(t, allocs)
		})
	}
}

func BenchmarkRewriteResponseModel(b *testing.B) {
	jsonNoop := []byte(`{"model":"client-model","response":{"model":"client-model"}}`)
	jsonRewrite := []byte(`{"model":"upstream","response":{"model":"upstream"},"message":{"model":"upstream"}}`)
	sseNoop := sserelay.Event{Raw: []byte("data: {\"model\":\"client-model\"}\n\n"), Data: []byte(`{"model":"client-model"}`)}
	sseRewrite := sserelay.Event{Raw: []byte("event: message\ndata: {\"message\":{\"model\":\"upstream\"}}\n\n"), Data: []byte(`{"message":{"model":"upstream"}}`)}
	cases := []struct {
		name    string
		input   []byte
		rewrite func() []byte
	}{
		{name: "json/noop", input: jsonNoop, rewrite: func() []byte { return rewriteResponseModelJSON(jsonNoop, "client-model") }},
		{name: "json/rewrite", input: jsonRewrite, rewrite: func() []byte { return rewriteResponseModelJSON(jsonRewrite, "client-model") }},
		{name: "sse/noop", input: sseNoop.Raw, rewrite: func() []byte { return rewriteResponseModelSSE(sseNoop, "client-model") }},
		{name: "sse/rewrite", input: sseRewrite.Raw, rewrite: func() []byte { return rewriteResponseModelSSE(sseRewrite, "client-model") }},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.input)))
			for b.Loop() {
				modelRewriteSink = tc.rewrite()
			}
		})
	}
}

func rewriteModelSSEStream(t *testing.T, src, model string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	err := sserelay.Relay(context.Background(), recorder, strings.NewReader(src), sserelay.Config{
		Mapper: func(event sserelay.Event) ([]byte, bool) {
			return rewriteResponseModelSSE(event, model), false
		},
	})
	require.NoError(t, err)
	return recorder.Body.String()
}
