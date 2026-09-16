// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

// aiProjectHost is the azure.yaml host owned by this extension.
// A project service carries model deployments.
// It may also carry an endpoint for an existing project.
const aiProjectHost = "azure.ai.project"

const (
	serviceTargetPreviewRequestMetadataKey = "azd.serviceTarget.preview"
	serviceTargetPreviewResultMetadataKey  = "azd.serviceTarget.previewResult"
	serviceTargetPreviewArtifactLocation   = "azd://service-target-preview"
)

var _ azdext.ServiceTargetProvider = (*projectServiceTarget)(nil)

// projectServiceTarget owns the azure.ai.project host.
// The microsoft.foundry provider provisions projects, deployments,
// accounts, and RBAC. This target keeps deploy graph ordering but has
// no package, publish, or deploy work.
//
// When the entry sets `endpoint:`, provisioning reuses that project.
// This target still has nothing to upsert during deploy.
type projectServiceTarget struct {
	azdClient     *azdext.AzdClient
	serviceConfig *azdext.ServiceConfig
}

// newProjectServiceTarget creates the project service target.
func newProjectServiceTarget(azdClient *azdext.AzdClient) azdext.ServiceTargetProvider {
	return &projectServiceTarget{azdClient: azdClient}
}

// Initialize stores the service configuration.
func (p *projectServiceTarget) Initialize(ctx context.Context, serviceConfig *azdext.ServiceConfig) error {
	p.serviceConfig = serviceConfig
	return nil
}

// Endpoints returns no endpoints.
// Provisioning publishes the endpoint through the azd environment.
func (p *projectServiceTarget) Endpoints(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	targetResource *azdext.TargetResource,
) ([]string, error) {
	return nil, nil
}

// GetTargetResource delegates to azd's resolver.
// It returns a minimal target when the resolver cannot find one.
func (p *projectServiceTarget) GetTargetResource(
	ctx context.Context,
	subscriptionId string,
	serviceConfig *azdext.ServiceConfig,
	defaultResolver func() (*azdext.TargetResource, error),
) (*azdext.TargetResource, error) {
	if defaultResolver != nil {
		if target, err := defaultResolver(); err == nil && target != nil {
			return target, nil
		}
	}
	return &azdext.TargetResource{SubscriptionId: subscriptionId}, nil
}

// Package is a no-op; the project has nothing to build or stage.
func (p *projectServiceTarget) Package(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	serviceContext *azdext.ServiceContext,
	progress azdext.ProgressReporter,
) (*azdext.ServicePackageResult, error) {
	return &azdext.ServicePackageResult{}, nil
}

// Publish is a no-op; the project has no artifact to publish.
func (p *projectServiceTarget) Publish(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	serviceContext *azdext.ServiceContext,
	targetResource *azdext.TargetResource,
	publishOptions *azdext.PublishOptions,
	progress azdext.ProgressReporter,
) (*azdext.ServicePublishResult, error) {
	return &azdext.ServicePublishResult{}, nil
}

// Deploy is a no-op because resources are provisioned earlier.
// Removing the service stops management without deleting resources.
// Teardown continues to run through `azd down`.
func (p *projectServiceTarget) Deploy(
	ctx context.Context,
	serviceConfig *azdext.ServiceConfig,
	serviceContext *azdext.ServiceContext,
	targetResource *azdext.TargetResource,
	progress azdext.ProgressReporter,
) (*azdext.ServiceDeployResult, error) {
	if targetResource != nil &&
		targetResource.GetMetadata()[serviceTargetPreviewRequestMetadataKey] == "true" {
		preview := struct {
			Target struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"target"`
			Source           string `json:"source"`
			Action           string `json:"action"`
			RemoteComparison string `json:"remoteComparison"`
			Changes          []any  `json:"changes"`
		}{
			Source:           "azure.yaml",
			Action:           "skip",
			RemoteComparison: "notApplicable",
			Changes:          []any{},
		}
		preview.Target.Type = "Microsoft Foundry project"
		preview.Target.Name = serviceConfig.GetName()
		payload, err := json.Marshal(preview)
		if err != nil {
			return nil, fmt.Errorf("serialize Foundry project deployment preview: %w", err)
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
	return &azdext.ServiceDeployResult{}, nil
}
