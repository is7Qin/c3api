// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRewriteConvertedFrames_CRLFMultiFramePreservesBoundaries(t *testing.T) {
	src := []byte("event: response.created\r\ndata: {\"response\":{\"model\":\"upstream-model\"}}\r\n\r\nevent: response.completed\r\ndata: {\"response\":{\"model\":\"upstream-model\"}}\r\n\r\ndata: [DONE]\r\n\r\n")
	got := rewriteConvertedFrames(src, "client-model")
	want := []byte("event: response.created\r\ndata: {\"response\":{\"model\":\"client-model\"}}\r\n\r\nevent: response.completed\r\ndata: {\"response\":{\"model\":\"client-model\"}}\r\n\r\ndata: [DONE]\r\n\r\n")
	require.Equal(t, string(want), string(got), "CRLF frames must remain separate with preserved boundaries")
	require.Equal(t, 3, bytes.Count(got, []byte("\r\n\r\n")), "three CRLF frame delimiters preserved")
	require.Equal(t, 2, bytes.Count(got, []byte("\"model\":\"client-model\"")), "both model-bearing frames rewritten")
	require.NotContains(t, string(got), "upstream-model")
	require.Contains(t, string(got), "data: [DONE]\r\n\r\n")
}

func TestRewriteConvertedFrames_CRLFPreservesMetadataAndMixedLineEndings(t *testing.T) {
	src := []byte(": comment keep\r\nid: 1\r\ndata: {\"response\":{\"model\":\"upstream-model\"}}\r\n\r\ndata: {\"response\":{\"model\":\"upstream-model\"}}\n\n")
	got := rewriteConvertedFrames(src, "client-model")
	require.Contains(t, string(got), ": comment keep\r\n", "metadata comment preserved with CRLF")
	require.Contains(t, string(got), "id: 1\r\n", "id line preserved with CRLF")
	require.Equal(t, 1, bytes.Count(got, []byte("\r\n\r\n")), "first frame CRLF delimiter preserved")
	require.Equal(t, 1, bytes.Count(got, []byte("\n\n")), "second frame LF delimiter preserved")
	require.Equal(t, 2, bytes.Count(got, []byte("client-model")))
}

func TestRewriteConvertedFrames_CRLFDoesNotConcatenate(t *testing.T) {
	src := []byte("data: {\"model\":\"upstream-model\",\"a\":1}\r\n\r\ndata: {\"model\":\"upstream-model\",\"b\":2}\r\n\r\n")
	got := rewriteConvertedFrames(src, "client-model")
	frames := bytes.Split(got, []byte("\r\n\r\n"))
	nonEmpty := 0
	for _, f := range frames {
		if len(bytes.TrimSpace(f)) > 0 {
			nonEmpty++
			require.Contains(t, string(f), "data:", "each split frame has data")
		}
	}
	require.Equal(t, 2, nonEmpty, "CRLF frames must not be concatenated into one")
}
