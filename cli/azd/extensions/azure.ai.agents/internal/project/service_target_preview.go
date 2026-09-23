// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"azureaiagent/internal/exterrors"
	"azureaiagent/internal/pkg/agents/agent_api"
	"azureaiagent/internal/pkg/agents/agent_yaml"
	"azureaiagent/internal/pkg/projectconfig"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

var _ azdext.ServiceTargetPreviewProvider = (*AgentServiceTargetProvider)(nil)

type agentPreviewReader interface {
	GetAgent(context.Context, string, string, bool) (*agent_api.AgentObject, error)
}

// Preview reads the current project and remote agent without initializing the
// deployment lifecycle. All output is returned to the host, never printed.
func (p *AgentServiceTargetProvider) Preview(
	ctx context.Context, service *azdext.ServiceConfig,
) (*azdext.ServiceDeployPreviewResult, error) {
	project, err := p.azdClient.Project().Get(ctx, &azdext.EmptyRequest{})
	if err != nil || project.GetProject().GetPath() == "" {
		return nil, exterrors.Dependency(exterrors.CodeProjectNotFound,
			"Cannot read the project for deployment preview.", "run preview from an initialized azd project")
	}
	// The request may have already resolved includes. Classify the original
	// project entry as well so transport normalization cannot erase provenance.
	original := project.Project.GetServices()[service.GetName()]
	if original == nil {
		return nil, previewConfigurationError()
	}
	if _, _, err := resolvePreviewDefinition(original, project.Project.Path); err != nil {
		return nil, err
	}
	service, definition, err := resolvePreviewDefinition(service, project.Project.Path)
	if err != nil {
		return nil, err
	}
	current, err := p.azdClient.Environment().GetCurrent(ctx, &azdext.EmptyRequest{})
	if err != nil || current.GetEnvironment().GetName() == "" {
		return nil, exterrors.Dependency(exterrors.CodeEnvironmentNotFound,
			"An existing azd environment is required for deployment preview.",
			"select an existing environment with --environment")
	}
	values, err := p.azdClient.Environment().GetValues(ctx, &azdext.GetEnvironmentRequest{
		Name: current.Environment.Name,
	})
	if err != nil || values == nil {
		return nil, exterrors.Dependency(exterrors.CodeEnvironmentValuesFailed,
			"Cannot read environment values for deployment preview.", "check the selected azd environment")
	}
	environment := make(map[string]string, len(values.KeyValues))
	for _, entry := range values.KeyValues {
		environment[entry.GetKey()] = entry.GetValue()
	}
	endpoint, err := previewProjectEndpoint(environment["FOUNDRY_PROJECT_ENDPOINT"])
	if err != nil {
		return nil, err
	}
	pending, err := previewPendingEnvironment(project.Project.Path, service, environment)
	if err != nil {
		return nil, err
	}
	request, unknown, err := preparePreviewRequest(service, definition, environment)
	if err != nil {
		return nil, err
	}
	unknown = append(unknown, pending...)
	subscription := environment["AZURE_SUBSCRIPTION_ID"]
	if subscription == "" {
		return nil, exterrors.Dependency(exterrors.CodeMissingAzureSubscription,
			"AZURE_SUBSCRIPTION_ID is required for deployment preview.", "check the selected azd environment")
	}
	tenant, err := p.azdClient.Account().LookupTenant(ctx, &azdext.LookupTenantRequest{SubscriptionId: subscription})
	if err != nil || tenant.GetTenantId() == "" {
		return nil, exterrors.Auth(exterrors.CodeTenantLookupFailed,
			"Cannot resolve the user access tenant for deployment preview.",
			"run 'azd auth login' and check your subscription access")
	}
	factory := p.previewReader
	if factory == nil {
		factory = newAgentPreviewReader
	}
	reader, err := factory(endpoint, tenant.TenantId)
	if err != nil {
		return nil, exterrors.Auth(exterrors.CodeCredentialCreationFailed,
			"Cannot create the preview credential.", "run 'azd auth login'")
	}
	return previewAgentRequest(ctx, reader, service.Name, request, unknown)
}

func newAgentPreviewReader(endpoint, tenantID string) (agentPreviewReader, error) {
	credential, err := azidentity.NewAzureDeveloperCLICredential(&azidentity.AzureDeveloperCLICredentialOptions{
		TenantID: tenantID, AdditionallyAllowedTenants: []string{"*"},
	})
	if err != nil {
		return nil, err
	}
	return agent_api.NewAgentClient(endpoint, credential), nil
}

func previewProjectEndpoint(raw string) (string, error) {
	if raw == "" {
		return "", exterrors.Dependency(exterrors.CodeMissingAiProjectEndpoint,
			"FOUNDRY_PROJECT_ENDPOINT is required for deployment preview.",
			"connect this environment to an existing Microsoft Foundry project")
	}
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		return "", exterrors.Validation(exterrors.CodeInvalidParameter,
			"Invalid Foundry project endpoint for deployment preview.", "provide an HTTPS project endpoint")
	}
	endpoint.User = nil
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return strings.TrimRight(endpoint.String(), "/"), nil
}

func preparePreviewRequest(
	service *azdext.ServiceConfig, definition agent_yaml.ContainerAgent, environment map[string]string,
) (*agent_api.CreateAgentRequest, []string, error) {
	var unknown []string
	var options []agent_yaml.AgentBuildOption
	switch {
	case definition.CodeConfiguration != nil:
		unknown = append(unknown, "code")
	case definition.Image != "" && (service.GetDocker().GetImagePassthrough() ||
		definition.RegistryConnectionID != "" ||
		strings.EqualFold(strings.TrimSpace(environment["AZD_AGENT_SKIP_ACR"]), "true")):
		options = append(options, agent_yaml.WithImageURL(definition.Image))
	default:
		// A future build/push result is unknowable here. Never manufacture a
		// supposedly deployable tag, or mistake the source image for its output.
		options = append(options, agent_yaml.WithImageURL("preview.invalid/unknown"))
		unknown = append(unknown, "definition.container_configuration.image")
	}
	prepared, err := prepareDeployRequest(service, definition, environment, options)
	if err != nil {
		return nil, nil, previewConfigurationError()
	}
	return prepared.request, unknown, nil
}

func previewPendingEnvironment(
	root string, service *azdext.ServiceConfig, environment map[string]string,
) ([]string, error) {
	raw, err := projectconfig.LoadServiceLevelEnvironment(root, service.Name)
	if err != nil {
		return nil, previewConfigurationError()
	}
	var pending []string
	for name, expression := range raw {
		missing := map[string]bool{}
		expanded, err := ExpandEnv(expression, func(variable string) string {
			value, found := environment[variable]
			if !found {
				value, found = os.LookupEnv(variable)
			}
			if !found {
				missing[variable] = true
			}
			return value
		})
		if err != nil {
			return nil, previewConfigurationError()
		}
		unresolved := len(missing) > 0 && expanded == "" && service.Environment[name] == ""
		for _, match := range previewRequiredVariable.FindAllStringSubmatch(expression, -1) {
			unresolved = unresolved || missing[match[1]] || missing[match[2]]
		}
		if unresolved {
			pending = append(pending, "definition.environment_variables."+name)
		}
	}
	return pending, nil
}

var previewRequiredVariable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func previewAgentRequest(
	ctx context.Context, reader agentPreviewReader, service string,
	request *agent_api.CreateAgentRequest, unknown []string,
) (*azdext.ServiceDeployPreviewResult, error) {
	existing, err := reader.GetAgent(ctx, request.Name, agent_api.AgentEndpointAPIVersion,
		activityProfileFromCreateRequest(request).IsActivity)
	if err != nil {
		response, isResponse := errors.AsType[*azcore.ResponseError](err)
		if !isResponse || response.StatusCode != http.StatusNotFound {
			// Do not include response bodies, transport URLs, or credential errors:
			// any can contain secrets, even when the SDK's body logging is off.
			if isResponse {
				return nil, &azdext.ServiceError{
					Message:    fmt.Sprintf("Foundry deployment preview read failed (HTTP %d).", response.StatusCode),
					StatusCode: response.StatusCode, ServiceName: "foundry",
					Suggestion: "check your login, project permissions, and network access",
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("cannot read the deployed agent; check authentication, connectivity, and the response format")
		}
		existing = nil
	} else if existing == nil || existing.Name != request.Name || existing.Versions.Latest.Version == "" ||
		existing.Versions.Latest.Definition == nil {
		return nil, fmt.Errorf("Foundry returned a malformed agent response; preview cannot compare it")
	}
	return comparePreviewRequest(service, request, existing, unknown)
}
