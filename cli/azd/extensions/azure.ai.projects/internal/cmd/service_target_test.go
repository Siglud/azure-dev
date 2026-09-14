// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"testing"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/require"
)

func TestProjectServiceTargetDeployPreviewReportsSkip(t *testing.T) {
	t.Parallel()

	target := &projectServiceTarget{}
	result, err := target.Deploy(
		t.Context(),
		&azdext.ServiceConfig{Name: "ai-project"},
		&azdext.ServiceContext{},
		&azdext.TargetResource{Metadata: map[string]string{
			serviceTargetPreviewRequestMetadataKey: "true",
		}},
		func(string) {},
	)
	require.NoError(t, err)
	require.Len(t, result.Artifacts, 1)
	artifact := result.Artifacts[0]
	require.Equal(t, azdext.ArtifactKind_ARTIFACT_KIND_CONFIG, artifact.Kind)
	require.Equal(t, serviceTargetPreviewArtifactLocation, artifact.Location)

	var preview struct {
		Target struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"target"`
		Action           string `json:"action"`
		RemoteComparison string `json:"remoteComparison"`
		Changes          []any  `json:"changes"`
	}
	require.NoError(t, json.Unmarshal(
		[]byte(artifact.Metadata[serviceTargetPreviewResultMetadataKey]),
		&preview,
	))
	require.Equal(t, "Microsoft Foundry project", preview.Target.Type)
	require.Equal(t, "ai-project", preview.Target.Name)
	require.Equal(t, "skip", preview.Action)
	require.Equal(t, "notApplicable", preview.RemoteComparison)
	require.Empty(t, preview.Changes)
}

func TestProjectServiceTargetDeployRemainsNoOp(t *testing.T) {
	t.Parallel()

	target := &projectServiceTarget{}
	result, err := target.Deploy(
		t.Context(),
		&azdext.ServiceConfig{Name: "ai-project"},
		&azdext.ServiceContext{},
		nil,
		func(string) {},
	)
	require.NoError(t, err)
	require.Empty(t, result.Artifacts)
}
