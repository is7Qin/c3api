// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func modelMappingTemplate(name string, mapping domain.ModelMapping) *domain.Template {
	return &domain.Template{
		Name:             name,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     mapping,
	}
}

func TestModelMappingServiceNormalizesOmittedCreateAndPut(t *testing.T) {
	service := &Service{store: newFakeStore(), inv: &invRecorder{}}
	created, err := service.CreateTemplate(context.Background(), modelMappingTemplate("mapping-normalized", nil))
	require.NoError(t, err)
	require.NotNil(t, created.ModelMapping)
	require.Empty(t, created.ModelMapping)

	created.ModelMapping = nil
	updated, err := service.UpdateTemplate(context.Background(), created)
	require.NoError(t, err)
	require.NotNil(t, updated.ModelMapping)
	require.Empty(t, updated.ModelMapping)
}

func TestModelMappingServiceRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name    string
		mapping domain.ModelMapping
	}{
		{"whitespace alias", domain.ModelMapping{" alias": {MappedModel: "target", Mode: domain.ModelMappingModeExplicit}}},
		{"whitespace target", domain.ModelMapping{"alias": {MappedModel: " target", Mode: domain.ModelMappingModeExplicit}}},
		{"invalid mode", domain.ModelMapping{"alias": {MappedModel: "target", Mode: domain.ModelMappingModeInvalid}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{store: newFakeStore(), inv: &invRecorder{}}
			_, err := service.CreateTemplate(context.Background(), modelMappingTemplate("mapping-invalid", tc.mapping))
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestModelMappingServiceBatchOmissionPreserves(t *testing.T) {
	service := &Service{store: newFakeStore(), inv: &invRecorder{}}
	want := domain.ModelMapping{"before": {MappedModel: "target-before", Mode: domain.ModelMappingModeExplicit}}
	created, err := service.CreateTemplate(context.Background(), modelMappingTemplate("mapping-preserve", want))
	require.NoError(t, err)
	name := "mapping-preserved"
	require.NoError(t, service.UpdateTemplatesBatch(context.Background(), []int64{created.ID}, repository.TemplatePatch{Name: &name}))
	fetched, err := service.GetTemplate(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, want, fetched.ModelMapping)
}

func TestModelMappingServiceBatchObjectReplaces(t *testing.T) {
	service := &Service{store: newFakeStore(), inv: &invRecorder{}}
	created, err := service.CreateTemplate(context.Background(), modelMappingTemplate("mapping-replace", domain.ModelMapping{"before": {MappedModel: "target-before", Mode: domain.ModelMappingModeExplicit}}))
	require.NoError(t, err)
	want := domain.ModelMapping{"after": {MappedModel: "target-after", Mode: domain.ModelMappingModeImplicit}}
	require.NoError(t, service.UpdateTemplatesBatch(context.Background(), []int64{created.ID}, repository.TemplatePatch{ModelMapping: &want}))
	fetched, err := service.GetTemplate(context.Background(), created.ID)
	require.NoError(t, err)
	require.Equal(t, want, fetched.ModelMapping)
}

func TestModelMappingServiceBatchEmptyClears(t *testing.T) {
	service := &Service{store: newFakeStore(), inv: &invRecorder{}}
	created, err := service.CreateTemplate(context.Background(), modelMappingTemplate("mapping-clear", domain.ModelMapping{"before": {MappedModel: "target-before", Mode: domain.ModelMappingModeExplicit}}))
	require.NoError(t, err)
	empty := domain.ModelMapping{}
	require.NoError(t, service.UpdateTemplatesBatch(context.Background(), []int64{created.ID}, repository.TemplatePatch{ModelMapping: &empty}))
	fetched, err := service.GetTemplate(context.Background(), created.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched.ModelMapping)
	require.Empty(t, fetched.ModelMapping)
}
