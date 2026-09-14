// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"azureaiagent/internal/exterrors"
	"azureaiagent/internal/pkg/agents/agent_api"
	"azureaiagent/internal/pkg/agents/agent_yaml"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// These values mirror the service-target preview contract in the azd SDK.
// The extension intentionally builds against the latest released SDK, so the
// wire constants are repeated here until the next SDK dependency update.
const (
	serviceTargetPreviewRequestMetadataKey = "azd.serviceTarget.preview"
	serviceTargetPreviewResultMetadataKey  = "azd.serviceTarget.previewResult"
	serviceTargetPreviewEnvironmentKey     = "azd.serviceTarget.previewEnvironment"
	serviceTargetPreviewArtifactLocation   = "azd://service-target-preview"
)

// AgentDeployPlan is the service-target preview payload for a hosted agent.
type AgentDeployPlan struct {
	Target           AgentDeployTarget        `json:"target"`
	Source           string                   `json:"source,omitempty"`
	Action           string                   `json:"action"`
	RemoteComparison string                   `json:"remoteComparison,omitempty"`
	RemoteVersion    string                   `json:"remoteVersion,omitempty"`
	Changes          []AgentDeployChange      `json:"changes"`
	Artifact         *AgentDeployArtifactPlan `json:"artifact,omitempty"`
}

// AgentDeployTarget identifies the hosted agent represented by a preview.
type AgentDeployTarget struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// AgentDeployChange describes one configuration difference in a preview.
type AgentDeployChange struct {
	Group  string `json:"group"`
	Field  string `json:"field"`
	Change string `json:"change"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// AgentDeployArtifactPlan describes artifact work a deployment would perform.
type AgentDeployArtifactPlan struct {
	Type        string `json:"type"`
	Reference   string `json:"reference,omitempty"`
	Digest      string `json:"digest,omitempty"`
	WouldBuild  bool   `json:"wouldBuild"`
	WouldPush   bool   `json:"wouldPush"`
	WouldUpload bool   `json:"wouldUpload"`
}

// AgentLookup retrieves an agent without mutating it.
type AgentLookup func(context.Context, string, bool) (*agent_api.AgentObject, error)

// AgentDeployPlanOptions configures a hosted-agent deployment preview.
type AgentDeployPlanOptions struct {
	ServiceConfig   *azdext.ServiceConfig
	ProjectRoot     string
	DefinitionPath  string
	ProjectEndpoint string
	Environment     map[string]string
	Lookup          AgentLookup
}

// PlanAgentDeploy validates the local hosted-agent definition and compares it
// with the latest deployed version when a project endpoint is available.
func PlanAgentDeploy(ctx context.Context, options AgentDeployPlanOptions) (*AgentDeployPlan, error) {
	if options.ServiceConfig == nil {
		return nil, exterrors.Validation(
			exterrors.CodeInvalidServiceConfig,
			"service configuration is required for deployment preview",
			"run `azd deploy <service> --preview` from an initialized azd project",
		)
	}

	if err := ResolveServiceConfigInPlace(options.ServiceConfig, options.ProjectRoot); err != nil {
		return nil, exterrors.Validation(
			exterrors.CodeInvalidServiceConfig,
			fmt.Sprintf("failed to resolve service config for %s: %s", options.ServiceConfig.Name, err),
			"fix the agent service configuration in azure.yaml",
		)
	}
	agentDef, isHosted, source, sourceName, err := loadDeployPreviewDefinition(options)
	if err != nil {
		return nil, err
	}
	if !isHosted {
		return nil, exterrors.Validation(
			exterrors.CodeUnsupportedAgentKind,
			"deployment preview currently supports hosted agents only",
			"select an azure.ai.agent service with kind: hosted",
		)
	}
	if source.IsLegacy() {
		WarnLegacyAgentShape(source)
	}
	if err := validateEnvironmentVariableNames(
		options.ServiceConfig.GetEnvironment(),
		agentDef.EnvironmentVariables,
	); err != nil {
		return nil, err
	}
	if err := validateRegistryConnectionDefinition(agentDef); err != nil {
		return nil, err
	}
	if err := validateRegistryConnectionServiceConfig(options.ServiceConfig); err != nil {
		return nil, err
	}
	if agentDef.RegistryConnectionID != "" &&
		!options.ServiceConfig.GetDocker().GetImagePassthrough() {
		return nil, exterrors.Validation(
			exterrors.CodeInvalidServiceConfig,
			"registryConnectionId requires docker.imagePassthrough: true",
			"enable docker.imagePassthrough for the private pre-built image",
		)
	}

	serviceTargetConfig, err := LoadServiceTargetAgentConfig(options.ServiceConfig)
	if err != nil {
		return nil, exterrors.Validation(
			exterrors.CodeInvalidServiceConfig,
			fmt.Sprintf("failed to parse service target config: %s", err),
			"check the service configuration in azure.yaml",
		)
	}
	if err := validateMemoryStores(serviceTargetConfig.MemoryStores); err != nil {
		return nil, err
	}
	activityProfile, err := ResolveActivityProfileForDeploy(agentDef, serviceTargetConfig.Activity)
	if err != nil {
		return nil, exterrors.Validation(
			exterrors.CodeInvalidServiceConfig,
			fmt.Sprintf("invalid Activity configuration: %s", err),
			"check the activity configuration in azure.yaml",
		)
	}
	if len(serviceTargetConfig.MemoryStores) > 0 {
		return nil, exterrors.Validation(
			exterrors.CodeUnsupportedDeployPreview,
			"deployment preview does not yet support hosted agents that provision memory stores",
			"preview an agent without memoryStores, or run `azd deploy` to apply the complete deployment",
		)
	}
	if activityProfile.IsActivity && activityProfile.UseCase == ActivityUseCaseSimple {
		return nil, exterrors.Validation(
			exterrors.CodeUnsupportedDeployPreview,
			"deployment preview does not yet support simple Activity agents that manage an Azure Bot",
			"preview a non-Activity or Digital Worker agent, or run `azd deploy` to apply the complete deployment",
		)
	}

	resolvedEnvironment, err := resolveDeployPreviewEnvironment(
		agentDef,
		options.ServiceConfig.GetEnvironment(),
		options.Environment,
	)
	if err != nil {
		return nil, err
	}

	cpu, memory := deployPreviewResources(serviceTargetConfig)
	artifact, buildOptions := deployPreviewArtifact(
		agentDef,
		options.ServiceConfig,
		options.Environment,
	)
	buildOptions = append(
		[]agent_yaml.AgentBuildOption{
			agent_yaml.WithEnvironmentVariables(resolvedEnvironment),
			agent_yaml.WithCPU(cpu),
			agent_yaml.WithMemory(memory),
		},
		buildOptions...,
	)

	request, err := agent_yaml.CreateAgentAPIRequestFromDefinition(agentDef, buildOptions...)
	if err != nil {
		return nil, exterrors.Validation(
			exterrors.CodeInvalidAgentRequest,
			fmt.Sprintf("failed to create agent request from definition: %s", err),
			"fix the agent definition and retry",
		)
	}
	applyAgentMetadata(request)
	if serviceTargetConfig.Activity != nil &&
		serviceTargetConfig.Activity.DigitalWorkerType == agent_api.DigitalWorkerTypeM365 {
		request.DigitalWorkerType = agent_api.DigitalWorkerTypeM365
	}
	ensureActivityEndpointAuthSchemeForProfile(request, activityProfile)

	plan := &AgentDeployPlan{
		Target: AgentDeployTarget{
			Type: "Microsoft Foundry hosted agent",
			Name: request.Name,
		},
		Source:           sourceName,
		Action:           "create",
		RemoteComparison: "unavailableUntilProvision",
		Changes:          desiredAgentStateChanges(request),
		Artifact:         &artifact,
	}

	projectEndpoint := strings.TrimRight(strings.TrimSpace(options.ProjectEndpoint), "/")
	if projectEndpoint == "" {
		return plan, nil
	}
	if options.Lookup == nil {
		return nil, exterrors.Internal(
			exterrors.CodeAgentCreateFailed,
			"deployment preview cannot compare remote state without an agent lookup client",
		)
	}

	current, err := options.Lookup(ctx, request.Name, activityProfile.IsActivity)
	if err != nil {
		if responseError, ok := errors.AsType[*azcore.ResponseError](err); ok &&
			responseError.StatusCode == http.StatusNotFound {
			plan.RemoteComparison = "notFound"
			return plan, nil
		}
		return nil, redactURLCredentialsInError(
			exterrors.ServiceFromAzure(err, exterrors.OpGetAgent),
			options.ProjectEndpoint,
		)
	}
	if current == nil {
		return nil, exterrors.Internal(
			exterrors.CodeAgentCreateFailed,
			"deployment preview received an empty agent response",
		)
	}
	if activityProfile.IsActivity {
		if _, err := ResolveDeployedActivityProfile(activityProfile, current.DigitalWorkerType); err != nil {
			return nil, exterrors.Validation(
				exterrors.CodeInvalidServiceConfig,
				err.Error(),
				digitalWorkerTypeMismatchSuggestion(),
			)
		}
	}

	plan.Action = "createVersion"
	plan.RemoteComparison = "compared"
	plan.RemoteVersion = current.Versions.Latest.Version
	plan.Changes, err = compareAgentDeployState(request, current, artifact)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *AgentServiceTargetProvider) previewDeploy(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	targetResource *azdext.TargetResource,
) (*azdext.ServiceDeployResult, error) {
	if p.projectPath == "" {
		projectResponse, err := p.azdClient.Project().Get(ctx, nil)
		if err != nil {
			return nil, exterrors.Dependency(
				exterrors.CodeProjectNotFound,
				fmt.Sprintf("failed to get project for deployment preview: %s", err),
				"run `azd init` to initialize your project",
			)
		}
		p.projectPath = projectResponse.GetProject().GetPath()
		p.projectServices = projectResponse.GetProject().GetServices()
	}
	environment, err := previewEnvironmentFromTarget(targetResource)
	if err != nil {
		return nil, err
	}

	projectEndpoint := firstNonEmpty(
		environment["FOUNDRY_PROJECT_ENDPOINT"],
		environment["AZURE_AI_PROJECT_ENDPOINT"],
		environment["AZURE_AIPROJECT_ENDPOINT"],
	)
	var lookup AgentLookup
	if projectEndpoint != "" {
		credential, err := p.previewCredential(ctx, environment["AZURE_SUBSCRIPTION_ID"])
		if err != nil {
			return nil, err
		}
		client := agent_api.NewAgentClient(projectEndpoint, credential)
		lookup = func(ctx context.Context, name string, includeDigitalWorkerType bool) (*agent_api.AgentObject, error) {
			return client.GetAgent(
				ctx,
				name,
				agent_api.AgentEndpointAPIVersion,
				includeDigitalWorkerType,
			)
		}
	}

	plan, err := PlanAgentDeploy(ctx, AgentDeployPlanOptions{
		ServiceConfig:   serviceConfig,
		ProjectRoot:     p.projectPath,
		DefinitionPath:  os.Getenv("AGENT_DEFINITION_PATH"),
		ProjectEndpoint: projectEndpoint,
		Environment:     environment,
		Lookup:          lookup,
	})
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("serialize deployment preview: %w", err)
	}
	return &azdext.ServiceDeployResult{
		Artifacts: []*azdext.Artifact{{
			Kind:         azdext.ArtifactKind_ARTIFACT_KIND_CONFIG,
			Location:     serviceTargetPreviewArtifactLocation,
			LocationKind: azdext.LocationKind_LOCATION_KIND_REMOTE,
			Metadata: map[string]string{
				serviceTargetPreviewResultMetadataKey: string(payload),
			},
		}},
	}, nil
}

func loadDeployPreviewDefinition(
	options AgentDeployPlanOptions,
) (agent_yaml.ContainerAgent, bool, AgentDefinitionSource, string, error) {
	definitionPath := strings.TrimSpace(options.DefinitionPath)
	if definitionPath == "" {
		agentDef, isHosted, source, err := LoadAgentDefinition(options.ServiceConfig, options.ProjectRoot)
		if err != nil {
			return agent_yaml.ContainerAgent{},
				false,
				source,
				"",
				redactURLCredentialsInError(err, deployPreviewSensitiveValues(options)...)
		}

		return agentDef, isHosted, source, deployPreviewSource(source), nil
	}

	extension := strings.ToLower(filepath.Ext(definitionPath))
	if extension != ".yaml" && extension != ".yml" {
		return agent_yaml.ContainerAgent{}, false, AgentDefinitionSourceDisk, "", exterrors.Validation(
			exterrors.CodeAgentDefinitionNotFound,
			fmt.Sprintf("agent definition file must be a YAML file (.yaml or .yml), got: %s", definitionPath),
			"provide a file with .yaml or .yml extension",
		)
	}
	data, err := os.ReadFile(definitionPath) //nolint:gosec // explicit user-provided override
	if err != nil {
		return agent_yaml.ContainerAgent{}, false, AgentDefinitionSourceDisk, "", exterrors.Validation(
			exterrors.CodeInvalidAgentManifest,
			fmt.Sprintf("failed to read agent definition file: %s", err),
			"verify AGENT_DEFINITION_PATH points to a readable agent definition",
		)
	}
	var rawDefinition struct {
		Image string `yaml:"image"`
	}
	_ = yaml.Unmarshal(data, &rawDefinition)
	agentDef, isHosted, err := parseContainerAgentYAML(data)
	if err != nil {
		err = redactURLCredentialsInError(err, rawDefinition.Image)
	}
	return agentDef, isHosted, AgentDefinitionSourceDisk, filepath.Base(definitionPath), err
}

func previewEnvironmentFromTarget(targetResource *azdext.TargetResource) (map[string]string, error) {
	if !isServiceTargetPreviewRequest(targetResource) {
		return nil, exterrors.Internal(
			exterrors.CodeInvalidServiceConfig,
			"deployment preview request marker is missing",
		)
	}
	payload := targetResource.GetMetadata()[serviceTargetPreviewEnvironmentKey]
	if payload == "" {
		return nil, exterrors.Internal(
			exterrors.CodeEnvironmentValuesFailed,
			"deployment preview environment snapshot is missing",
		)
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(payload), &environment); err != nil {
		return nil, exterrors.InternalFromError(
			err,
			exterrors.CodeEnvironmentValuesFailed,
			"deployment preview environment snapshot is invalid",
		)
	}
	return environment, nil
}

func (p *AgentServiceTargetProvider) previewCredential(
	ctx context.Context,
	subscriptionID string,
) (azcore.TokenCredential, error) {
	options := &azidentity.AzureDeveloperCLICredentialOptions{
		AdditionallyAllowedTenants: []string{"*"},
	}
	if subscriptionID != "" {
		tenantResponse, err := p.azdClient.Account().LookupTenant(ctx, &azdext.LookupTenantRequest{
			SubscriptionId: subscriptionID,
		})
		if err != nil {
			return nil, exterrors.Auth(
				exterrors.CodeTenantLookupFailed,
				fmt.Sprintf("failed to get tenant ID for subscription %s: %s", subscriptionID, err),
				"verify your Azure login with `azd auth login` and that you have access to this subscription",
			)
		}
		options.TenantID = tenantResponse.TenantId
	}

	credential, err := azidentity.NewAzureDeveloperCLICredential(options)
	if err != nil {
		return nil, exterrors.Auth(
			exterrors.CodeCredentialCreationFailed,
			fmt.Sprintf("failed to create Azure credential: %s", err),
			"run `azd auth login` to authenticate",
		)
	}
	return credential, nil
}

func isServiceTargetPreviewRequest(targetResource *azdext.TargetResource) bool {
	return targetResource != nil &&
		targetResource.GetMetadata()[serviceTargetPreviewRequestMetadataKey] == "true"
}

func resolveDeployPreviewEnvironment(
	agentDef agent_yaml.ContainerAgent,
	serviceEnvironment map[string]string,
	azdEnvironment map[string]string,
) (map[string]string, error) {
	resolved := maps.Clone(serviceEnvironment)
	if resolved == nil {
		resolved = map[string]string{}
	}
	if agentDef.EnvironmentVariables == nil {
		return resolved, nil
	}
	for _, variable := range *agentDef.EnvironmentVariables {
		if _, found := resolved[variable.Name]; found {
			continue
		}
		value, err := ResolveAgentEnvironmentVariable(
			variable.Name,
			variable.Value,
			serviceEnvironment,
			func(name string) string { return azdEnvironment[name] },
		)
		if err != nil {
			return nil, exterrors.Validation(
				exterrors.CodeInvalidAgentManifest,
				fmt.Sprintf("failed to resolve environment variable %s: %s", variable.Name, err),
				"fix the environment-variable expression in the agent definition",
			)
		}
		resolved[variable.Name] = value
	}
	return resolved, nil
}

func deployPreviewResources(config *ServiceTargetAgentConfig) (string, string) {
	cpu, memory := DefaultCpu, DefaultMemory
	if config != nil && config.Container != nil && config.Container.Resources != nil {
		if config.Container.Resources.Cpu != "" {
			cpu = config.Container.Resources.Cpu
		}
		if config.Container.Resources.Memory != "" {
			memory = config.Container.Resources.Memory
		}
	}
	return cpu, memory
}

func deployPreviewArtifact(
	agentDef agent_yaml.ContainerAgent,
	serviceConfig *azdext.ServiceConfig,
	environment map[string]string,
) (AgentDeployArtifactPlan, []agent_yaml.AgentBuildOption) {
	if agentDef.CodeConfiguration != nil {
		return AgentDeployArtifactPlan{
			Type:        "codePackage",
			WouldUpload: true,
		}, nil
	}

	prebuilt := agentDef.RegistryConnectionID != "" ||
		serviceConfig.GetDocker().GetImagePassthrough() ||
		(agentDef.Image != "" && strings.EqualFold(strings.TrimSpace(environment["AZD_AGENT_SKIP_ACR"]), "true"))
	if prebuilt {
		return AgentDeployArtifactPlan{
				Type:      "containerImage",
				Reference: redactDeployPreviewURL(agentDef.Image),
			},
			[]agent_yaml.AgentBuildOption{agent_yaml.WithImageURL(agentDef.Image)}
	}

	return AgentDeployArtifactPlan{
			Type:       "containerImage",
			WouldBuild: true,
			WouldPush:  true,
		},
		[]agent_yaml.AgentBuildOption{agent_yaml.WithImageURL("<image-built-from-local-source>")}
}

func deployPreviewSource(source AgentDefinitionSource) string {
	if source == AgentDefinitionSourceDisk {
		return "agent.yaml"
	}
	return "azure.yaml"
}

func desiredAgentStateChanges(request *agent_api.CreateAgentRequest) []AgentDeployChange {
	desired, err := hostedDefinition(request.Definition)
	if err != nil {
		return []AgentDeployChange{}
	}

	changes := []AgentDeployChange{{
		Group: "metadata", Field: "name", Change: "add", After: request.Name,
	}}
	changes = appendDesiredValueChange(
		changes, "metadata", "description", pointerString(request.Description),
	)
	changes = appendDesiredValueChange(changes, "metadata", "metadata", request.Metadata)
	changes = appendDesiredValueChange(
		changes,
		"protocols",
		"protocolVersions",
		sortedProtocols(desired.ProtocolVersions),
	)
	changes = appendDesiredValueChange(changes, "resources", "cpu", desired.CPU)
	changes = appendDesiredValueChange(changes, "resources", "memory", desired.Memory)
	changes = append(changes, createEnvironmentChanges(desired.EnvironmentVariables)...)
	changes = appendDesiredValueChange(changes, "configuration", "raiPolicy", desired.RaiConfig)
	changes = appendDesiredValueChange(
		changes,
		"configuration",
		"codeConfiguration",
		desired.CodeConfiguration,
	)
	changes = appendDesiredValueChange(
		changes,
		"configuration",
		"sessionConfiguration",
		desired.SessionConfiguration,
	)
	changes = appendDesiredValueChange(
		changes,
		"configuration",
		"registryConnectionId",
		registryConnectionID(desired),
	)
	changes = appendDesiredValueChange(changes, "configuration", "agentEndpoint", request.AgentEndpoint)
	changes = appendDesiredValueChange(changes, "configuration", "agentCard", request.AgentCard)
	changes = appendDesiredValueChange(
		changes,
		"configuration",
		"digitalWorkerType",
		request.DigitalWorkerType,
	)
	return changes
}

func compareAgentDeployState(
	desiredRequest *agent_api.CreateAgentRequest,
	current *agent_api.AgentObject,
	artifact AgentDeployArtifactPlan,
) ([]AgentDeployChange, error) {
	desired, err := hostedDefinition(desiredRequest.Definition)
	if err != nil {
		return nil, fmt.Errorf("decode desired hosted agent definition: %w", err)
	}
	existing, err := hostedDefinition(current.Versions.Latest.Definition)
	if err != nil {
		return nil, fmt.Errorf("decode deployed hosted agent definition: %w", err)
	}

	changes := []AgentDeployChange{}
	changes = appendValueChange(
		changes,
		"metadata",
		"description",
		pointerString(current.Versions.Latest.Description),
		pointerString(desiredRequest.Description),
	)
	changes = appendValueChange(
		changes,
		"metadata",
		"metadata",
		current.Versions.Latest.Metadata,
		desiredRequest.Metadata,
	)
	changes = appendValueChange(
		changes,
		"protocols",
		"protocolVersions",
		sortedProtocols(existing.ProtocolVersions),
		sortedProtocols(desired.ProtocolVersions),
	)
	changes = appendValueChange(changes, "resources", "cpu", existing.CPU, desired.CPU)
	changes = appendValueChange(changes, "resources", "memory", existing.Memory, desired.Memory)
	changes = append(changes, compareEnvironment(
		existing.EnvironmentVariables,
		desired.EnvironmentVariables,
	)...)
	changes = appendValueChange(
		changes,
		"configuration",
		"raiPolicy",
		existing.RaiConfig,
		desired.RaiConfig,
	)
	changes = appendValueChange(
		changes,
		"configuration",
		"codeConfiguration",
		existing.CodeConfiguration,
		desired.CodeConfiguration,
	)
	changes = appendValueChange(
		changes,
		"configuration",
		"sessionConfiguration",
		existing.SessionConfiguration,
		desired.SessionConfiguration,
	)
	changes = appendValueChange(
		changes,
		"configuration",
		"registryConnectionId",
		registryConnectionID(existing),
		registryConnectionID(desired),
	)
	if desiredRequest.AgentEndpoint != nil {
		changes = appendAgentEndpointChanges(
			changes,
			current.AgentEndpoint,
			desiredRequest.AgentEndpoint,
		)
	}
	if desiredRequest.AgentCard != nil {
		currentCard := current.AgentCard
		if currentCard != nil && desiredRequest.AgentCard.Version == nil {
			currentCardCopy := *currentCard
			currentCardCopy.Version = nil
			currentCard = &currentCardCopy
		}
		changes = appendValueChange(
			changes,
			"configuration",
			"agentCard",
			currentCard,
			desiredRequest.AgentCard,
		)
	}

	if artifact.Type == "containerImage" && !artifact.WouldBuild {
		changes = appendValueChange(
			changes,
			"artifact",
			"containerImage",
			redactDeployPreviewURL(containerImage(existing)),
			artifact.Reference,
		)
	}

	return changes, nil
}

func appendAgentEndpointChanges(
	changes []AgentDeployChange,
	current *agent_api.AgentEndpoint,
	desired *agent_api.AgentEndpoint,
) []AgentDeployChange {
	if desired == nil {
		return changes
	}
	var currentValue agent_api.AgentEndpoint
	if current != nil {
		currentValue = *current
	}
	if len(desired.Protocols) > 0 {
		changes = appendValueChange(
			changes,
			"configuration",
			"agentEndpoint.protocols",
			sortedEndpointProtocols(currentValue.Protocols),
			sortedEndpointProtocols(desired.Protocols),
		)
	}
	if desired.VersionSelector != nil {
		changes = appendValueChange(
			changes,
			"configuration",
			"agentEndpoint.versionSelector",
			currentValue.VersionSelector,
			desired.VersionSelector,
		)
	}
	if desired.ProtocolConfiguration != nil {
		changes = appendValueChange(
			changes,
			"configuration",
			"agentEndpoint.protocolConfiguration",
			currentValue.ProtocolConfiguration,
			desired.ProtocolConfiguration,
		)
	}
	if len(desired.AuthorizationSchemes) > 0 {
		changes = appendValueChange(
			changes,
			"configuration",
			"agentEndpoint.authorizationSchemes",
			currentValue.AuthorizationSchemes,
			desired.AuthorizationSchemes,
		)
	}
	return changes
}

func sortedEndpointProtocols(
	protocols []agent_api.AgentEndpointProtocol,
) []agent_api.AgentEndpointProtocol {
	result := slices.Clone(protocols)
	slices.Sort(result)
	return result
}

func hostedDefinition(value any) (agent_api.HostedAgentDefinition, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return agent_api.HostedAgentDefinition{}, err
	}
	var definition agent_api.HostedAgentDefinition
	if err := json.Unmarshal(data, &definition); err != nil {
		return agent_api.HostedAgentDefinition{}, err
	}
	return definition, nil
}

func appendValueChange(
	changes []AgentDeployChange,
	group string,
	field string,
	before any,
	after any,
) []AgentDeployChange {
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) == string(afterJSON) {
		return changes
	}

	change := "update"
	if isEmptyJSON(beforeJSON) {
		change = "add"
	} else if isEmptyJSON(afterJSON) {
		change = "remove"
	}
	return append(changes, AgentDeployChange{
		Group:  group,
		Field:  field,
		Change: change,
		Before: redactDeployPreviewValue(before),
		After:  redactDeployPreviewValue(after),
	})
}

func appendDesiredValueChange(
	changes []AgentDeployChange,
	group string,
	field string,
	after any,
) []AgentDeployChange {
	afterJSON, _ := json.Marshal(after)
	if isEmptyJSON(afterJSON) {
		return changes
	}
	return append(changes, AgentDeployChange{
		Group: group, Field: field, Change: "add", After: redactDeployPreviewValue(after),
	})
}

func isEmptyJSON(value []byte) bool {
	return string(value) == "null" || string(value) == `""` ||
		string(value) == "{}" || string(value) == "[]"
}

func createEnvironmentChanges(environment map[string]string) []AgentDeployChange {
	changes := make([]AgentDeployChange, 0, len(environment))
	for _, name := range slices.Sorted(maps.Keys(environment)) {
		changes = append(changes, AgentDeployChange{
			Group:  deployPreviewEnvironmentGroup(name),
			Field:  name,
			Change: "add",
			After:  deployPreviewEnvironmentValue(name, environment[name]),
		})
	}
	return changes
}

func compareEnvironment(before, after map[string]string) []AgentDeployChange {
	names := make(map[string]struct{}, len(before)+len(after))
	for name := range before {
		names[name] = struct{}{}
	}
	for name := range after {
		names[name] = struct{}{}
	}

	var changes []AgentDeployChange
	for _, name := range slices.Sorted(maps.Keys(names)) {
		beforeValue, beforeFound := before[name]
		afterValue, afterFound := after[name]
		if beforeFound == afterFound && beforeValue == afterValue {
			continue
		}

		change := "update"
		var displayedBefore any = deployPreviewEnvironmentValue(name, beforeValue)
		var displayedAfter any = deployPreviewEnvironmentValue(name, afterValue)
		if !beforeFound {
			change = "add"
			displayedBefore = nil
		} else if !afterFound {
			change = "remove"
			displayedAfter = nil
		}
		changes = append(changes, AgentDeployChange{
			Group:  deployPreviewEnvironmentGroup(name),
			Field:  name,
			Change: change,
			Before: displayedBefore,
			After:  displayedAfter,
		})
	}
	return changes
}

func deployPreviewEnvironmentGroup(name string) string {
	switch name {
	case "AZURE_AI_MODEL_DEPLOYMENT_NAME", "FOUNDRY_MODEL_DEPLOYMENT_NAME":
		return "modelDeployment"
	default:
		return "environment"
	}
}

func deployPreviewEnvironmentValue(name, value string) string {
	if deployPreviewEnvironmentGroup(name) == "modelDeployment" {
		return value
	}
	return "<redacted>"
}

func sortedProtocols(protocols []agent_api.ProtocolVersionRecord) []agent_api.ProtocolVersionRecord {
	result := slices.Clone(protocols)
	slices.SortFunc(result, func(a, b agent_api.ProtocolVersionRecord) int {
		if protocolComparison := cmp.Compare(a.Protocol, b.Protocol); protocolComparison != 0 {
			return protocolComparison
		}
		return cmp.Compare(a.Version, b.Version)
	})
	return result
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func containerImage(definition agent_api.HostedAgentDefinition) string {
	if definition.ContainerConfiguration == nil {
		return ""
	}
	return definition.ContainerConfiguration.Image
}

func registryConnectionID(definition agent_api.HostedAgentDefinition) string {
	if definition.ContainerConfiguration == nil {
		return ""
	}
	return definition.ContainerConfiguration.RegistryConnectionID
}

func redactDeployPreviewURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	parsed, err := url.Parse(value)
	if err == nil && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}

	value, _, _ = strings.Cut(value, "?")
	value, _, _ = strings.Cut(value, "#")
	if at := strings.LastIndex(value, "@"); at >= 0 {
		firstSlash := strings.IndexAny(value, `/\`)
		if (firstSlash == -1 || at < firstSlash) && strings.Contains(value[:at], ":") {
			return value[at+1:]
		}
	}
	return value
}

var (
	deployPreviewURLPattern      = regexp.MustCompile(`https?://[^\s"'<>]+`)
	deployPreviewUserInfoPattern = regexp.MustCompile(`[^\s"'<>/:]+:[^@\s"'<>]+@[^\s"'<>]+`)
)

func redactDeployPreviewValue(value any) any {
	switch value := value.(type) {
	case nil, bool, float64:
		return value
	case string:
		return redactDeployPreviewText(value)
	case map[string]string:
		redacted := make(map[string]string, len(value))
		for key, item := range value {
			redacted[redactDeployPreviewText(key)] = redactDeployPreviewText(item)
		}
		return redacted
	case map[string]any:
		redacted := make(map[string]any, len(value))
		for key, item := range value {
			redacted[redactDeployPreviewText(key)] = redactDeployPreviewValue(item)
		}
		return redacted
	case []string:
		redacted := make([]string, len(value))
		for i, item := range value {
			redacted[i] = redactDeployPreviewText(item)
		}
		return redacted
	case []any:
		redacted := make([]any, len(value))
		for i, item := range value {
			redacted[i] = redactDeployPreviewValue(item)
		}
		return redacted
	}

	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return value
	}
	return redactDeployPreviewValue(normalized)
}

func redactDeployPreviewText(value string) string {
	value = deployPreviewURLPattern.ReplaceAllStringFunc(value, redactDeployPreviewURL)
	return deployPreviewUserInfoPattern.ReplaceAllStringFunc(value, redactDeployPreviewURL)
}

func deployPreviewSensitiveValues(options AgentDeployPlanOptions) []string {
	values := []string{options.ServiceConfig.GetImage()}
	for _, properties := range []*structpb.Struct{
		options.ServiceConfig.GetAdditionalProperties(),
		options.ServiceConfig.GetConfig(),
	} {
		if value := properties.GetFields()["image"].GetStringValue(); value != "" {
			values = append(values, value)
		}
	}
	if strings.TrimSpace(options.DefinitionPath) != "" {
		return values
	}
	for _, name := range []string{"agent.yaml", "agent.yml"} {
		path := filepath.Join(
			options.ProjectRoot,
			options.ServiceConfig.GetRelativePath(),
			name,
		)
		data, err := os.ReadFile(path) //nolint:gosec // project-relative compatibility definition
		if err != nil {
			continue
		}
		var rawDefinition struct {
			Image string `yaml:"image"`
		}
		if yaml.Unmarshal(data, &rawDefinition) == nil && rawDefinition.Image != "" {
			values = append(values, rawDefinition.Image)
		}
		break
	}
	return values
}

func redactURLCredentialsInError(err error, values ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, value := range values {
		redacted := redactDeployPreviewURL(value)
		if value != "" && value != redacted {
			message = strings.ReplaceAll(message, value, redacted)
		}
	}
	message = redactDeployPreviewText(message)
	if localError, ok := errors.AsType[*azdext.LocalError](err); ok {
		redacted := *localError
		redacted.Message = message
		return &redacted
	}
	if serviceError, ok := errors.AsType[*azdext.ServiceError](err); ok {
		redacted := *serviceError
		redacted.Message = message
		return &redacted
	}
	return errors.New(message)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
