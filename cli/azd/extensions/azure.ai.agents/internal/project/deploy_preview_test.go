// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"azureaiagent/internal/exterrors"
	"azureaiagent/internal/pkg/agents/agent_api"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestPlanAgentDeployUnifiedFreshProject(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(projectRoot, "main.py"),
		[]byte("print('ready')\n"),
		0o600,
	))
	service := previewService(t, map[string]any{ // #nosec G101 -- Credential-bearing URLs test redaction.
		"kind":        "hosted",
		"name":        "fresh-agent",
		"description": "Fresh agent: https://user:password@example.test/readme?sig=secret",
		"metadata": map[string]any{
			"docs": "https://user:password@example.test/docs?sig=secret#fragment", // #nosec G101 -- Redaction fixture.
		},
		"protocols": []any{
			map[string]any{"protocol": "responses", "version": "2.0.0"},
		},
		"codeConfiguration": map[string]any{
			"runtime":    "python_3_13",
			"entryPoint": "main.py",
		},
		"container": map[string]any{
			"resources": map[string]any{"cpu": "0.5", "memory": "1Gi"},
		},
	})
	service.Environment = map[string]string{
		"AZURE_AI_MODEL_DEPLOYMENT_NAME": "gpt-5-mini",
		"API_KEY":                        "do-not-print",
	}

	lookupCalled := false
	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: service,
		ProjectRoot:   projectRoot,
		Lookup: func(context.Context, string, bool) (*agent_api.AgentObject, error) {
			lookupCalled = true
			return nil, nil
		},
	})
	require.NoError(t, err)
	require.False(t, lookupCalled)
	require.Equal(t, AgentDeployTarget{
		Type: "Microsoft Foundry hosted agent",
		Name: "fresh-agent",
	}, plan.Target)
	require.Equal(t, "azure.yaml", plan.Source)
	require.Equal(t, "create", plan.Action)
	require.Equal(t, "unavailableUntilProvision", plan.RemoteComparison)
	require.Equal(t, "codePackage", plan.Artifact.Type)
	require.True(t, plan.Artifact.WouldUpload)
	require.NotEmpty(t, plan.Changes)

	data, err := json.Marshal(plan)
	require.NoError(t, err)
	require.NotContains(t, string(data), "do-not-print")
	require.NotContains(t, string(data), "password")
	require.NotContains(t, string(data), "sig=secret")
	require.Contains(t, string(data), "redacted")
	require.Contains(t, string(data), "gpt-5-mini")

	entries, err := os.ReadDir(projectRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1, "preview must not create package or state files")
}

func TestPlanAgentDeployLegacyProjectUsesAgentYamlBesideImportManifest(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(projectRoot, "agent.yaml"),
		[]byte("kind: hosted\nname: legacy-agent\nlanguage: python\n"),
		0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(projectRoot, "agent.manifest.yaml"),
		[]byte("name: legacy-sample\ntemplate:\n  kind: hosted\n"),
		0o600,
	))

	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: &azdext.ServiceConfig{
			Name: "legacy-agent", Host: "azure.ai.agent", RelativePath: ".",
		},
		ProjectRoot: projectRoot,
	})
	require.NoError(t, err)
	require.Equal(t, "legacy-agent", plan.Target.Name)
	require.Equal(t, "agent.yaml", plan.Source)
	require.Equal(t, "unavailableUntilProvision", plan.RemoteComparison)
	require.True(t, plan.Artifact.WouldBuild)
	require.True(t, plan.Artifact.WouldPush)

	require.NoError(t, os.Remove(filepath.Join(projectRoot, "agent.manifest.yaml")))
	plan, err = PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: &azdext.ServiceConfig{
			Name: "legacy-agent", Host: "azure.ai.agent", RelativePath: ".",
		},
		ProjectRoot: projectRoot,
	})
	require.NoError(t, err, "legacy deploy must not require agent.manifest.yaml")
	require.Equal(t, "agent.yaml", plan.Source)
}

func TestPlanAgentDeploySupportsCurrentRefAndDeprecatedNestedConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		service func(*testing.T, string) *azdext.ServiceConfig
	}{
		{
			name: "RootRef",
			service: func(t *testing.T, root string) *azdext.ServiceConfig {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "definition.yaml"),
					[]byte(`
kind: hosted
name: referenced-agent
codeConfiguration:
  runtime: python_3_13
  entryPoint: main.py
`),
					0o600,
				))
				return previewService(t, map[string]any{
					"name": "referenced-agent",
					"$ref": "./definition.yaml",
				})
			},
		},
		{
			name: "DeprecatedConfig",
			service: func(t *testing.T, _ string) *azdext.ServiceConfig {
				config, err := structpb.NewStruct(map[string]any{
					"kind": "hosted",
					"name": "nested-agent",
					"codeConfiguration": map[string]any{
						"runtime":    "python_3_13",
						"entryPoint": "main.py",
					},
				})
				require.NoError(t, err)
				return &azdext.ServiceConfig{
					Name: "nested-agent", Host: "azure.ai.agent", RelativePath: ".",
					Config: config,
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
				ServiceConfig: test.service(t, root),
				ProjectRoot:   root,
			})
			require.NoError(t, err)
			require.Equal(t, "azure.yaml", plan.Source)
			require.Equal(t, "codePackage", plan.Artifact.Type)
		})
	}
}

func TestPlanAgentDeployHonorsDefinitionPathOverride(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	overridePath := filepath.Join(root, "override.yml")
	require.NoError(t, os.WriteFile(
		overridePath,
		[]byte("kind: hosted\nname: override-agent\nlanguage: python\n"),
		0o600,
	))

	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: previewService(t, map[string]any{
			"kind": "hosted",
			"name": "inline-agent",
		}),
		ProjectRoot:    root,
		DefinitionPath: overridePath,
	})
	require.NoError(t, err)
	require.Equal(t, "override-agent", plan.Target.Name)
	require.Equal(t, "override.yml", plan.Source)
}

func TestPlanAgentDeploySourceContainerWouldBuildAndPush(t *testing.T) {
	t.Parallel()

	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: previewService(t, map[string]any{
			"kind": "hosted",
			"name": "container-agent",
		}),
		ProjectRoot: t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, "containerImage", plan.Artifact.Type)
	require.True(t, plan.Artifact.WouldBuild)
	require.True(t, plan.Artifact.WouldPush)
	require.False(t, plan.Artifact.WouldUpload)
	require.Empty(t, plan.Artifact.Reference)
}

func TestPlanAgentDeployRegistryConnectionUsesPrebuiltImage(t *testing.T) {
	t.Parallel()

	service := previewService(t, map[string]any{
		"kind":                 "hosted",
		"name":                 "private-agent",
		"registryConnectionId": "private-registry",
	})
	service.Image = "registry.example.com/team/agent:v1"
	service.Docker = &azdext.DockerProjectOptions{ImagePassthrough: true}

	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: service,
		ProjectRoot:   t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, "registry.example.com/team/agent:v1", plan.Artifact.Reference)
	require.False(t, plan.Artifact.WouldBuild)
	require.False(t, plan.Artifact.WouldPush)
}

func TestPlanAgentDeployRejectsUnplannedCompanionResources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		properties map[string]any
		want       string
	}{
		{
			name: "MemoryStore",
			properties: map[string]any{
				"kind": "hosted",
				"name": "memory-agent",
				"codeConfiguration": map[string]any{
					"runtime":    "python_3_13",
					"entryPoint": "main.py",
				},
				"memoryStores": []any{map[string]any{
					"name":           "memory",
					"chatModel":      "gpt-5-mini",
					"embeddingModel": "text-embedding-3-small",
				}},
			},
			want: "does not yet support hosted agents that provision memory stores",
		},
		{
			name: "SimpleActivityAgent",
			properties: map[string]any{
				"kind": "hosted",
				"name": "activity-agent",
				"protocols": []any{map[string]any{
					"protocol": "activity",
					"version":  "2.0.0",
				}},
				"codeConfiguration": map[string]any{
					"runtime":    "python_3_13",
					"entryPoint": "main.py",
				},
			},
			want: "does not yet support simple Activity agents that manage an Azure Bot",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
				ServiceConfig: previewService(t, test.properties),
				ProjectRoot:   t.TempDir(),
			})
			require.ErrorContains(t, err, test.want)
			localError, ok := errors.AsType[*azdext.LocalError](err)
			require.True(t, ok)
			require.Equal(t, exterrors.CodeUnsupportedDeployPreview, localError.Code)
		})
	}
}

func TestPlanAgentDeployRemoteNotFound(t *testing.T) {
	t.Parallel()

	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: previewService(t, map[string]any{
			"kind": "hosted",
			"name": "new-agent",
			"codeConfiguration": map[string]any{
				"runtime":    "python_3_13",
				"entryPoint": "main.py",
			},
		}),
		ProjectRoot:     t.TempDir(),
		ProjectEndpoint: "https://account.services.ai.azure.com/api/projects/project",
		Lookup: func(context.Context, string, bool) (*agent_api.AgentObject, error) {
			return nil, &azcore.ResponseError{StatusCode: 404}
		},
	})
	require.NoError(t, err)
	require.Equal(t, "create", plan.Action)
	require.Equal(t, "notFound", plan.RemoteComparison)
}

func TestPlanAgentDeployNoConfigurationChanges(t *testing.T) {
	t.Parallel()

	projectRoot := t.TempDir()
	service := previewService(t, map[string]any{
		"kind": "hosted",
		"name": "existing-agent",
		"protocols": []any{
			map[string]any{"protocol": "responses", "version": "2.0.0"},
		},
		"container": map[string]any{
			"resources": map[string]any{"cpu": "0.5", "memory": "1Gi"},
		},
	})
	service.Image = "registry.example.com/team/agent:v1"
	service.Docker = &azdext.DockerProjectOptions{ImagePassthrough: true}

	current := &agent_api.AgentObject{
		Name: "existing-agent",
		AgentEndpoint: &agent_api.AgentEndpoint{
			Protocols: []agent_api.AgentEndpointProtocol{agent_api.AgentEndpointProtocolResponses},
		},
		AgentCard: &agent_api.AgentCard{Description: "preserved remote card"},
	}
	current.Versions.Latest = agent_api.AgentVersionObject{
		Name:     "existing-agent",
		Version:  "7",
		Metadata: map[string]string{"enableVnextExperience": "true"},
		Definition: agent_api.HostedAgentDefinition{
			AgentDefinition: agent_api.AgentDefinition{Kind: agent_api.AgentKindHosted},
			ProtocolVersions: []agent_api.ProtocolVersionRecord{{
				Protocol: agent_api.AgentProtocolResponses,
				Version:  "2.0.0",
			}},
			CPU:    "0.5",
			Memory: "1Gi",
			ContainerConfiguration: &agent_api.ContainerConfigurationAPI{
				Image: "registry.example.com/team/agent:v1",
			},
		},
	}

	plan, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig:   service,
		ProjectRoot:     projectRoot,
		ProjectEndpoint: "https://account.services.ai.azure.com/api/projects/project",
		Lookup: func(_ context.Context, name string, _ bool) (*agent_api.AgentObject, error) {
			require.Equal(t, "existing-agent", name)
			return current, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, "createVersion", plan.Action)
	require.Equal(t, "compared", plan.RemoteComparison)
	require.Equal(t, "7", plan.RemoteVersion)
	require.Empty(t, plan.Changes)
	require.False(t, plan.Artifact.WouldBuild)
	require.False(t, plan.Artifact.WouldPush)
}

func TestCompareEnvironmentDetectsEmptyValueRemoval(t *testing.T) {
	t.Parallel()

	changes := compareEnvironment(map[string]string{"EMPTY": ""}, map[string]string{})

	require.Equal(t, []AgentDeployChange{{
		Group: "environment", Field: "EMPTY", Change: "remove",
		Before: "<redacted>", After: nil,
	}}, changes)
}

func TestCompareAgentDeployStateIncludesAgentLevelAndRaiChanges(t *testing.T) {
	t.Parallel()

	current := &agent_api.AgentObject{
		AgentEndpoint: &agent_api.AgentEndpoint{
			Protocols: []agent_api.AgentEndpointProtocol{agent_api.AgentEndpointProtocolResponses},
		},
		AgentCard: &agent_api.AgentCard{Description: "old"},
	}
	current.Versions.Latest = agent_api.AgentVersionObject{
		Definition: agent_api.HostedAgentDefinition{
			AgentDefinition: agent_api.AgentDefinition{
				Kind:      agent_api.AgentKindHosted,
				RaiConfig: &agent_api.RaiConfig{RaiPolicyName: "old-policy"},
			},
			CPU:    "0.5",
			Memory: "1Gi",
			ContainerConfiguration: &agent_api.ContainerConfigurationAPI{
				Image:                "registry.example.com/agent:v1",
				RegistryConnectionID: "old-connection",
			},
		},
	}
	desired := &agent_api.CreateAgentRequest{
		AgentEndpoint: &agent_api.AgentEndpoint{
			Protocols: []agent_api.AgentEndpointProtocol{agent_api.AgentEndpointProtocolA2A},
		},
		AgentCard: &agent_api.AgentCard{Description: "new"},
		CreateAgentVersionRequest: agent_api.CreateAgentVersionRequest{
			Definition: agent_api.HostedAgentDefinition{
				AgentDefinition: agent_api.AgentDefinition{
					Kind:      agent_api.AgentKindHosted,
					RaiConfig: &agent_api.RaiConfig{RaiPolicyName: "new-policy"},
				},
				CPU:    "0.5",
				Memory: "1Gi",
				ContainerConfiguration: &agent_api.ContainerConfigurationAPI{
					Image:                "registry.example.com/agent:v2",
					RegistryConnectionID: "new-connection",
				},
			},
		},
	}

	changes, err := compareAgentDeployState(desired, current, AgentDeployArtifactPlan{
		Type:      "containerImage",
		Reference: "registry.example.com/agent:v2",
	})
	require.NoError(t, err)
	requirePreviewChange(t, changes, "configuration", "raiPolicy")
	requirePreviewChange(t, changes, "configuration", "agentEndpoint.protocols")
	requirePreviewChange(t, changes, "configuration", "agentCard")
	requirePreviewChange(t, changes, "configuration", "registryConnectionId")
	requirePreviewChange(t, changes, "artifact", "containerImage")
}

func TestRedactDeployPreviewURLRemovesCredentials(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"https://user:password@registry.example.com/" +
			"team/agent:v1?sig=secret#fragment": "https://registry.example.com/team/agent:v1",
		"user:password@registry.example.com/team/agent:v1?sig=secret": "registry.example.com/team/agent:v1",
		"localhost:5000/team/agent@sha256:abc":                        "localhost:5000/team/agent@sha256:abc",
	}
	for input, expected := range tests {
		require.Equal(t, expected, redactDeployPreviewURL(input))
	}
}

func TestPlanAgentDeployDoesNotDiscloseImageCredentials(t *testing.T) {
	t.Parallel()

	service := previewService(t, map[string]any{
		"kind": "hosted",
		"name": "private-image-agent",
	})
	// #nosec G101 -- Credential-bearing URL is required for the non-disclosure test.
	service.Image = "https://user:password@registry.example.com/team/agent:v1?sig=secret#fragment"
	service.Docker = &azdext.DockerProjectOptions{ImagePassthrough: true}

	_, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
		ServiceConfig: service,
		ProjectRoot:   t.TempDir(),
	})
	require.Error(t, err)
	output := err.Error()
	require.NotContains(t, output, "user")
	require.NotContains(t, output, "password")
	require.NotContains(t, output, "sig=secret")
	require.NotContains(t, output, "fragment")
	require.Contains(t, output, "https://registry.example.com/team/agent:v1")
}

func TestAgentServiceTargetInitializeDoesNotDiscloseImageCredentials(t *testing.T) {
	t.Parallel()

	service := previewService(t, map[string]any{
		"kind":                 "hosted",
		"name":                 "private-image-agent",
		"registryConnectionId": "private-registry",
	})
	service.Image = "user:password@registry.example.com/team/agent?sig=secret#fragment"
	service.Docker = &azdext.DockerProjectOptions{ImagePassthrough: true}

	err := (&AgentServiceTargetProvider{}).Initialize(t.Context(), service)

	require.Error(t, err)
	message := err.Error()
	require.NotContains(t, message, "user")
	require.NotContains(t, message, "password")
	require.NotContains(t, message, "sig=secret")
	require.NotContains(t, message, "fragment")
	require.Contains(t, message, "registry.example.com/team/agent")
}

func TestAgentServiceTargetInitializeDoesNotDiscloseLegacyImageCredentials(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "agent.yaml"),
		[]byte(
			"kind: hosted\nname: legacy-agent\n"+
				"image: user:password@registry.example.com/team/agent?sig=secret#fragment\n",
		),
		0o600,
	))
	provider := &AgentServiceTargetProvider{
		azdClient: newEndpointsTestClient(t, root, map[string]string{}),
	}
	service := &azdext.ServiceConfig{
		Name:         "legacy-agent",
		Host:         "azure.ai.agent",
		RelativePath: ".",
		Image:        "registry.example.com/fallback:v1",
	}

	err := provider.Initialize(t.Context(), service)

	require.Error(t, err)
	message := err.Error()
	require.NotContains(t, message, "user")
	require.NotContains(t, message, "password")
	require.NotContains(t, message, "sig=secret")
	require.NotContains(t, message, "fragment")
	require.Contains(t, message, "registry.example.com/team/agent")
}

func TestPlanAgentDeployRedactsDefinitionSourceImageErrors(t *testing.T) {
	t.Parallel()

	const sensitiveImage = "user:password@registry.example.com/team/agent?sig=secret#fragment"
	tests := []struct {
		name    string
		service func(*testing.T, string) *azdext.ServiceConfig
	}{
		{
			name: "LegacyAgentYaml",
			service: func(t *testing.T, root string) *azdext.ServiceConfig {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "agent.yaml"),
					[]byte("kind: hosted\nname: legacy-agent\nimage: "+sensitiveImage+"\n"),
					0o600,
				))
				return &azdext.ServiceConfig{
					Name: "legacy-agent", Host: "azure.ai.agent", RelativePath: ".",
				}
			},
		},
		{
			name: "DeprecatedNestedConfig",
			service: func(t *testing.T, _ string) *azdext.ServiceConfig {
				config, err := structpb.NewStruct(map[string]any{
					"kind":  "hosted",
					"name":  "nested-agent",
					"image": sensitiveImage,
				})
				require.NoError(t, err)
				return &azdext.ServiceConfig{
					Name: "nested-agent", Host: "azure.ai.agent", RelativePath: ".",
					Config: config,
				}
			},
		},
		{
			name: "ReferencedDefinition",
			service: func(t *testing.T, root string) *azdext.ServiceConfig {
				require.NoError(t, os.WriteFile(
					filepath.Join(root, "definition.yaml"),
					[]byte("kind: hosted\nname: referenced-agent\nimage: "+sensitiveImage+"\n"),
					0o600,
				))
				return previewService(t, map[string]any{
					"name": "referenced-agent",
					"$ref": "./definition.yaml",
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			_, err := PlanAgentDeploy(t.Context(), AgentDeployPlanOptions{
				ServiceConfig: test.service(t, root),
				ProjectRoot:   root,
			})
			require.Error(t, err)
			message := err.Error()
			require.NotContains(t, message, "user")
			require.NotContains(t, message, "password")
			require.NotContains(t, message, "sig=secret")
			require.NotContains(t, message, "fragment")
		})
	}
}

func TestIsServiceTargetPreviewRequest(t *testing.T) {
	t.Parallel()

	require.False(t, isServiceTargetPreviewRequest(nil))
	require.True(t, isServiceTargetPreviewRequest(&azdext.TargetResource{
		Metadata: map[string]string{serviceTargetPreviewRequestMetadataKey: "true"},
	}))
}

func TestAgentServiceTargetDeployPreviewReturnsPlanWithoutDeploymentState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "main.py"),
		[]byte("print('ready')\n"),
		0o600,
	))
	service := previewService(t, map[string]any{
		"kind": "hosted",
		"name": "fresh-agent",
		"codeConfiguration": map[string]any{
			"runtime":    "python_3_13",
			"entryPoint": "main.py",
		},
	})
	provider := &AgentServiceTargetProvider{
		azdClient: newEndpointsTestClient(t, root, map[string]string{}),
	}

	result, err := provider.Deploy(
		t.Context(),
		service,
		&azdext.ServiceContext{},
		&azdext.TargetResource{
			Metadata: map[string]string{
				serviceTargetPreviewRequestMetadataKey: "true",
				serviceTargetPreviewEnvironmentKey:     "{}",
			},
		},
		func(string) {},
	)
	require.NoError(t, err)
	require.Len(t, result.Artifacts, 1)
	payload := result.Artifacts[0].Metadata[serviceTargetPreviewResultMetadataKey]
	require.NotEmpty(t, payload)

	var plan AgentDeployPlan
	require.NoError(t, json.Unmarshal([]byte(payload), &plan))
	require.Equal(t, "create", plan.Action)
	require.Equal(t, "unavailableUntilProvision", plan.RemoteComparison)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Len(t, entries, 1, "preview must not write package, environment, or deployment state")
	require.Equal(t, "main.py", entries[0].Name())
}

func TestPreviewEnvironmentFromTarget(t *testing.T) {
	t.Parallel()

	environment, err := previewEnvironmentFromTarget(&azdext.TargetResource{
		Metadata: map[string]string{
			serviceTargetPreviewRequestMetadataKey: "true",
			serviceTargetPreviewEnvironmentKey:     `{"AZURE_ENV_NAME":"production"}`,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "production", environment["AZURE_ENV_NAME"])
}

func previewService(t *testing.T, properties map[string]any) *azdext.ServiceConfig {
	t.Helper()
	props, err := structpb.NewStruct(properties)
	require.NoError(t, err)
	return &azdext.ServiceConfig{
		Name:                 properties["name"].(string),
		Host:                 "azure.ai.agent",
		RelativePath:         ".",
		AdditionalProperties: props,
	}
}

func requirePreviewChange(
	t *testing.T,
	changes []AgentDeployChange,
	group string,
	field string,
) {
	t.Helper()
	require.Contains(t, changes, AgentDeployChange{
		Group:  group,
		Field:  field,
		Change: "update",
		Before: findPreviewValue(changes, group, field, true),
		After:  findPreviewValue(changes, group, field, false),
	})
}

func findPreviewValue(changes []AgentDeployChange, group, field string, before bool) any {
	for _, change := range changes {
		if change.Group == group && change.Field == field {
			if before {
				return change.Before
			}
			return change.After
		}
	}
	return nil
}
