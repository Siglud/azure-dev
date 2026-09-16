// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package azdext

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceTargetPreviewRequest(t *testing.T) {
	t.Parallel()

	target := &TargetResource{}
	require.False(t, IsServiceTargetPreviewRequest(target))

	MarkServiceTargetPreviewRequest(target)

	require.True(t, IsServiceTargetPreviewRequest(target))
	require.Equal(t, "true", target.Metadata[ServiceTargetPreviewRequestMetadataKey])
}

func TestServiceTargetPreviewEnvironmentRoundTrip(t *testing.T) {
	t.Parallel()

	target := &TargetResource{}
	MarkServiceTargetPreviewRequest(target)
	expected := map[string]string{
		"AZURE_ENV_NAME":           "dev",
		"FOUNDRY_PROJECT_ENDPOINT": "https://example.test",
	}
	require.NoError(t, SetServiceTargetPreviewEnvironment(target, expected))

	actual, err := ParseServiceTargetPreviewEnvironment(target)
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}

func TestServiceTargetPreviewResultArtifactRoundTrip(t *testing.T) {
	t.Parallel()

	expected := &ServiceTargetPreview{
		Target:           ServiceTargetPreviewTarget{Type: "hosted agent", Name: "assistant"},
		Source:           "azure.yaml",
		Action:           "createVersion",
		RemoteComparison: "compared",
		RemoteVersion:    "7",
		Changes: []ServiceTargetPreviewChange{{
			Group: "resources", Field: "cpu", Change: "update", Before: "0.5", After: "1",
		}},
		Artifact: &ServiceTargetPreviewArtifact{
			Type:       "containerImage",
			WouldBuild: true,
			WouldPush:  true,
		},
	}

	artifact, err := NewServiceTargetPreviewResultArtifact(expected)
	require.NoError(t, err)
	require.Equal(t, ArtifactKind_ARTIFACT_KIND_CONFIG, artifact.Kind)
	require.Equal(t, ServiceTargetPreviewArtifactLocation, artifact.Location)

	actual, err := ParseServiceTargetPreviewResult([]*Artifact{artifact})
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}

func TestParseServiceTargetPreviewResultRejectsInvalidPayload(t *testing.T) {
	t.Parallel()

	_, err := ParseServiceTargetPreviewResult([]*Artifact{{
		Metadata: map[string]string{ServiceTargetPreviewResultMetadataKey: "{"},
	}})
	require.ErrorContains(t, err, "parse service target preview result")
}

func TestParseServiceTargetPreviewResultRequiresIdentity(t *testing.T) {
	t.Parallel()

	artifact, err := NewServiceTargetPreviewResultArtifact(&ServiceTargetPreview{})
	require.NoError(t, err)

	_, err = ParseServiceTargetPreviewResult([]*Artifact{artifact})
	require.ErrorContains(t, err, "target type, target name, and action are required")
}

func TestParseServiceTargetPreviewResultRequiresArtifact(t *testing.T) {
	t.Parallel()

	_, err := ParseServiceTargetPreviewResult(nil)
	require.EqualError(t, err, "service target preview result not found")
}
