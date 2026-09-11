// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

func TestImagesRouteRefEditsVsGenerationsDistinct(t *testing.T) {
	gr := scheduler.RouteRefForOp(10, string(domain.FormatOpenAIImages), "gpt-image-1", domain.OpImagesGenerations)
	er := scheduler.RouteRefForOp(10, string(domain.FormatOpenAIImages), "gpt-image-1", domain.OpImagesEdits)
	// v4-S2: op-distinctness lives in the interned per-route hex (borrowed
	// from the published RouteDecision), not the normalized query key — the
	// key carries group/format/model/op only. Key-level distinction of the two
	// ops is covered by the select_session parity corpus (images op-tags case).
	require.NotEqual(t, gr.OperationTag, er.OperationTag)
	require.Equal(t, string(domain.OpImagesGenerations), gr.OperationTag)
	require.Equal(t, string(domain.OpImagesEdits), er.OperationTag)
	// legacy RouteRefFor must remain generations for compatibility
	legacy := scheduler.RouteRefFor(10, string(domain.FormatOpenAIImages), "gpt-image-1")
	require.Equal(t, string(domain.OpImagesGenerations), legacy.OperationTag)
	require.Equal(t, gr.OperationTag, legacy.OperationTag, "generations must stay generational")
}

func TestImagesCallerTypedDiscriminator(t *testing.T) {
	p := &Proxy{
		imageGenerations: &imagesCaller{path: "images/generations", op: domain.OpImagesGenerations},
		imageEdits:       &imagesCaller{path: "images/edits", op: domain.OpImagesEdits},
	}
	require.Equal(t, domain.OpImagesGenerations, p.imageGenerations.operationTag())
	require.Equal(t, domain.OpImagesEdits, p.imageEdits.operationTag())
	require.NotEqual(t, p.imageGenerations.operationTag(), p.imageEdits.operationTag())
}
