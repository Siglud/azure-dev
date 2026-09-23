// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package grpcserver

import (
	"testing"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	v1beta "github.com/azure/azure-dev/cli/azd/pkg/azdext/contracts/v1beta"
	"github.com/azure/azure-dev/cli/azd/pkg/extensions"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/ioc"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
	"github.com/azure/azure-dev/cli/azd/pkg/prompt"
	"github.com/azure/azure-dev/cli/azd/test/mocks/mockinput"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

type previewRegistrationPrompter struct {
	prompt.Prompter
}

func TestBetaServiceTargetServiceRegistersPreviewThroughSharedAdapter(t *testing.T) {
	extension := &extensions.Extension{
		Id:           "test.target",
		Capabilities: []extensions.CapabilityType{extensions.ServiceTargetProviderCapability},
	}
	manager := newStreamTestExtensionManager(t, extension)
	container := ioc.NewNestedContainer(nil)
	ioc.RegisterInstance[input.Console](container, mockinput.NewMockConsole())
	ioc.RegisterInstance[prompt.Prompter](container, &previewRegistrationPrompter{})
	service := NewServiceTargetService(container, manager, nil).(*ServiceTargetService)
	adapter := &betaServiceTargetServiceAdapter{stable: service}
	stream := newRegisterStream(
		extensionClaimsContext(t.Context(), extension.Id),
		&v1beta.ServiceTargetMessage{
			MessageType: &v1beta.ServiceTargetMessage_RegisterServiceTargetRequest{
				RegisterServiceTargetRequest: &v1beta.RegisterServiceTargetRequest{
					Host: "test-target", SupportsPreview: true,
				},
			},
		},
	)
	require.NoError(t, adapter.Stream(stream))
	require.NotNil(t, stream.response.GetRegisterServiceTargetResponse())
	var target project.ServiceTarget
	require.NoError(t, container.ResolveNamed("test-target", &target))
	capability, ok := target.(project.ServiceTargetPreviewCapability)
	require.True(t, ok)
	require.True(t, capability.SupportsPreview())
	require.Empty(t, service.providerMap)
}

func TestDeploymentPreviewMessagesSurviveSharedBetaAdapter(t *testing.T) {
	t.Parallel()
	request := &azdext.ServiceTargetMessage{
		RequestId: "preview",
		MessageType: &azdext.ServiceTargetMessage_PreviewRequest{
			PreviewRequest: &azdext.ServiceTargetPreviewRequest{
				ServiceConfig: &azdext.ServiceConfig{Name: "agent", Host: "azure.ai.agent"},
			},
		},
	}
	betaRequest := new(v1beta.ServiceTargetMessage)
	require.NoError(t, transcodeStableResponse(request, betaRequest))
	require.Equal(t, "preview", betaRequest.RequestId)
	require.Equal(t, "agent", betaRequest.GetPreviewRequest().GetServiceConfig().Name)
	data, err := structpb.NewStruct(map[string]any{"operation": "create", "changes": []any{"image"}})
	require.NoError(t, err)
	betaResponse := &v1beta.ServiceTargetMessage{
		RequestId: "preview",
		MessageType: &v1beta.ServiceTargetMessage_PreviewResponse{
			PreviewResponse: &v1beta.ServiceTargetPreviewResponse{
				Result: &v1beta.ServiceDeployPreviewResult{Message: "Would create", Data: data},
			},
		},
	}
	response := new(azdext.ServiceTargetMessage)
	require.NoError(t, transcodeBetaStreamRequest(betaResponse, response))
	require.Equal(t, "Would create", response.GetPreviewResponse().GetResult().Message)
	require.Equal(t, data.AsMap(), response.GetPreviewResponse().GetResult().Data.AsMap())
}

func TestServiceTargetServiceRegistersPreviewCapability(t *testing.T) {
	t.Parallel()
	for _, supported := range []bool{false, true} {
		name := "unsupported"
		if supported {
			name = "supported"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			container := ioc.NewNestedContainer(nil)
			ioc.RegisterInstance[input.Console](container, mockinput.NewMockConsole())
			ioc.RegisterInstance[prompt.Prompter](container, &previewRegistrationPrompter{})
			service := NewServiceTargetService(container, nil, nil).(*ServiceTargetService)
			host := ""
			response, err := service.onRegisterRequest(
				t.Context(),
				&azdext.RegisterServiceTargetRequest{Host: "custom", SupportsPreview: supported},
				&extensions.Extension{Id: "test.extension"},
				nil,
				&host,
			)
			require.NoError(t, err)
			require.NotNil(t, response.GetRegisterServiceTargetResponse())
			require.Equal(t, "custom", host)

			var target project.ServiceTarget
			require.NoError(t, container.ResolveNamed(host, &target))
			capability, ok := target.(project.ServiceTargetPreviewCapability)
			require.True(t, ok)
			require.Equal(t, supported, capability.SupportsPreview())
			previewer, ok := target.(project.ServiceTargetPreviewer)
			require.True(t, ok)
			_, err = previewer.Preview(t.Context(), nil)
			if supported {
				require.ErrorContains(t, err, "service configuration is required")
			} else {
				require.ErrorContains(t, err, "does not support deployment preview")
			}
		})
	}
}
