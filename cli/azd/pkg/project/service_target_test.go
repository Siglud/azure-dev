// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/extensions"
	"github.com/azure/azure-dev/cli/azd/pkg/grpcbroker"
	"github.com/azure/azure-dev/cli/azd/pkg/lazy"
	"github.com/azure/azure-dev/cli/azd/pkg/osutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test the edge case of empty kind

// Test edge cases

func Test_BuiltInServiceTargetNames(t *testing.T) {
	names := builtInServiceTargetNames()
	require.NotEmpty(t, names)

	assert.Contains(t, names, "appservice")
	assert.Contains(t, names, "containerapp")
	assert.Contains(t, names, "function")
	assert.Contains(t, names, "staticwebapp")
	assert.Contains(t, names, "aks")
	assert.Contains(t, names, "ai.endpoint")
}

func Test_ParseServiceHost(t *testing.T) {
	t.Run("valid kinds", func(t *testing.T) {
		kinds := []ServiceTargetKind{
			AppServiceTarget, ContainerAppTarget, AzureFunctionTarget,
			StaticWebAppTarget, AksTarget, AiEndpointTarget,
		}
		for _, kind := range kinds {
			result, err := parseServiceHost(kind)
			require.NoError(t, err)
			assert.Equal(t, kind, result)
		}
	})

	t.Run("empty host returns error", func(t *testing.T) {
		_, err := parseServiceHost(ServiceTargetKind(""))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "host cannot be empty")
	})

	t.Run("custom/extension host allowed", func(t *testing.T) {
		result, err := parseServiceHost(ServiceTargetKind("custom-extension"))
		require.NoError(t, err)
		assert.Equal(t, ServiceTargetKind("custom-extension"), result)
	})
}

func Test_ResourceTypeMismatchError(t *testing.T) {
	err := resourceTypeMismatchError("myResource", "Microsoft.Web/sites", "Microsoft.App/containerApps")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "myResource")
	assert.Contains(t, err.Error(), "Microsoft.Web/sites")
	assert.Contains(t, err.Error(), "Microsoft.App/containerApps")
}

func Test_CheckResourceType(t *testing.T) {
	t.Run("matching type", func(t *testing.T) {
		resource := environment.NewTargetResource("sub", "rg", "myApp", "Microsoft.Web/sites")
		err := checkResourceType(resource, "Microsoft.Web/sites")
		require.NoError(t, err)
	})

	t.Run("case insensitive match", func(t *testing.T) {
		resource := environment.NewTargetResource("sub", "rg", "myApp", "microsoft.web/sites")
		err := checkResourceType(resource, "Microsoft.Web/sites")
		require.NoError(t, err)
	})

	t.Run("mismatched type", func(t *testing.T) {
		resource := environment.NewTargetResource("sub", "rg", "myApp", "Microsoft.Web/sites")
		err := checkResourceType(resource, "Microsoft.App/containerApps")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not match")
	})
}

func Test_NewExternalServiceTarget(t *testing.T) {
	target := NewExternalServiceTarget("test-target", ContainerAppTarget, nil, nil, nil, nil, nil)
	require.NotNil(t, target)
}

func TestExternalServiceTargetSupportsPreview(t *testing.T) {
	t.Parallel()

	target := NewExternalServiceTarget(
		"preview-target",
		ServiceTargetKind("preview-target"),
		&extensions.Extension{
			Capabilities: []extensions.CapabilityType{
				extensions.ServiceTargetProviderCapability,
				extensions.ServiceTargetPreviewCapability,
			},
		},
		nil,
		nil,
		nil,
		nil,
	)
	previewer, ok := target.(ServiceTargetPreviewer)
	require.True(t, ok)
	require.True(t, previewer.SupportsPreview())

	target = NewExternalServiceTarget(
		"deploy-only-target",
		ServiceTargetKind("deploy-only-target"),
		&extensions.Extension{
			Capabilities: []extensions.CapabilityType{
				extensions.ServiceTargetProviderCapability,
			},
		},
		nil,
		nil,
		nil,
		nil,
	)
	previewer, ok = target.(ServiceTargetPreviewer)
	require.True(t, ok)
	require.False(t, previewer.SupportsPreview())
	_, err := previewer.Preview(
		t.Context(),
		&ServiceConfig{Name: "api"},
		environment.New("dev"),
	)
	require.EqualError(t, err, `service target "deploy-only-target" does not support deployment previews`)
}

func TestExternalServiceTargetPreviewUsesProvidedEnvironment(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream := &serviceTargetPreviewTestStream{
		requests:  make(chan *azdext.ServiceTargetMessage, 2),
		responses: make(chan *azdext.ServiceTargetMessage, 2),
		done:      ctx.Done(),
	}
	broker := grpcbroker.NewMessageBroker(
		stream,
		azdext.NewServiceTargetEnvelope(),
		"preview-test",
		nil,
	)
	go func() {
		_ = broker.Run(ctx)
	}()
	require.NoError(t, broker.Ready(ctx))

	var lazyEnvironmentCalls atomic.Int32
	lazyEnvironment := lazy.NewLazy(func() (*environment.Environment, error) {
		lazyEnvironmentCalls.Add(1)
		return environment.NewWithValues("mutating", map[string]string{
			"SERVICE_VALUE": "wrong-environment",
		}), nil
	})
	target := NewExternalServiceTarget(
		"preview-target",
		ServiceTargetKind("preview-target"),
		&extensions.Extension{
			Capabilities: []extensions.CapabilityType{
				extensions.ServiceTargetProviderCapability,
				extensions.ServiceTargetPreviewCapability,
			},
		},
		broker,
		nil,
		nil,
		lazyEnvironment,
	)
	previewer := target.(ServiceTargetPreviewer)
	serviceConfig := &ServiceConfig{
		Name: "api",
		Environment: osutil.ExpandableMap{
			"FROM_ENV": osutil.NewExpandableString("${SERVICE_VALUE}"),
		},
	}
	previewEnvironment := environment.NewWithValues("preview", map[string]string{
		"SERVICE_VALUE": "snapshot-value",
	})

	require.NoError(t, previewer.InitializePreview(ctx, serviceConfig, previewEnvironment))
	preview, err := previewer.Preview(ctx, serviceConfig, previewEnvironment)
	require.NoError(t, err)
	require.Equal(t, "noChange", preview.Action)
	require.Zero(t, lazyEnvironmentCalls.Load())

	initializeRequest := <-stream.requests
	require.Equal(
		t,
		"snapshot-value",
		initializeRequest.GetInitializeRequest().GetServiceConfig().GetEnvironment()["FROM_ENV"],
	)
	deployRequest := <-stream.requests
	require.Equal(
		t,
		"snapshot-value",
		deployRequest.GetDeployRequest().GetServiceConfig().GetEnvironment()["FROM_ENV"],
	)
	snapshot, err := azdext.ParseServiceTargetPreviewEnvironment(
		deployRequest.GetDeployRequest().GetTargetResource(),
	)
	require.NoError(t, err)
	require.Equal(t, "snapshot-value", snapshot["SERVICE_VALUE"])
}

type serviceTargetPreviewTestStream struct {
	requests  chan *azdext.ServiceTargetMessage
	responses chan *azdext.ServiceTargetMessage
	done      <-chan struct{}
}

func (s *serviceTargetPreviewTestStream) Send(message *azdext.ServiceTargetMessage) error {
	s.requests <- message

	response := &azdext.ServiceTargetMessage{RequestId: message.RequestId}
	switch message.MessageType.(type) {
	case *azdext.ServiceTargetMessage_InitializeRequest:
		response.MessageType = &azdext.ServiceTargetMessage_InitializeResponse{
			InitializeResponse: &azdext.ServiceTargetInitializeResponse{},
		}
	case *azdext.ServiceTargetMessage_DeployRequest:
		artifact, err := azdext.NewServiceTargetPreviewResultArtifact(&azdext.ServiceTargetPreview{
			Target: azdext.ServiceTargetPreviewTarget{Type: "test", Name: "api"},
			Action: "noChange",
		})
		if err != nil {
			return err
		}
		response.MessageType = &azdext.ServiceTargetMessage_DeployResponse{
			DeployResponse: &azdext.ServiceTargetDeployResponse{
				Result: &azdext.ServiceDeployResult{Artifacts: []*azdext.Artifact{artifact}},
			},
		}
	default:
		return fmt.Errorf("unexpected service target message %T", message.MessageType)
	}

	select {
	case s.responses <- response:
		return nil
	case <-s.done:
		return context.Canceled
	}
}

func (s *serviceTargetPreviewTestStream) Recv() (*azdext.ServiceTargetMessage, error) {
	select {
	case response := <-s.responses:
		return response, nil
	case <-s.done:
		return nil, io.EOF
	}
}

// ---------- IgnoreFile method coverage for different targets ----------
func Test_ServiceTargetKind_IgnoreFile_Extended(t *testing.T) {
	assert.Equal(t, ".webappignore", AppServiceTarget.IgnoreFile())
	assert.Equal(t, ".funcignore", AzureFunctionTarget.IgnoreFile())
	assert.Equal(t, "", ContainerAppTarget.IgnoreFile())
	assert.Equal(t, "", StaticWebAppTarget.IgnoreFile())
	assert.Equal(t, "", AksTarget.IgnoreFile())
}

// ---------- SupportsDelayedProvisioning ----------
func Test_ServiceTargetKind_SupportsDelayedProvisioning_Extended(t *testing.T) {
	assert.True(t, AksTarget.SupportsDelayedProvisioning())
	assert.False(t, AppServiceTarget.SupportsDelayedProvisioning())
	assert.False(t, ContainerAppTarget.SupportsDelayedProvisioning())
}
