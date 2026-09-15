// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package azdext

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	// ServiceTargetPreviewRequestMetadataKey marks a service-target deploy request as read-only preview work.
	ServiceTargetPreviewRequestMetadataKey = "azd.serviceTarget.preview"
	// ServiceTargetPreviewResultMetadataKey stores the JSON preview result on the response artifact.
	ServiceTargetPreviewResultMetadataKey = "azd.serviceTarget.previewResult"
	// ServiceTargetPreviewEnvironmentMetadataKey stores the read-only azd environment snapshot for the provider.
	ServiceTargetPreviewEnvironmentMetadataKey = "azd.serviceTarget.previewEnvironment"
	// ServiceTargetPreviewArtifactLocation identifies the internal preview result artifact.
	ServiceTargetPreviewArtifactLocation = "azd://service-target-preview"
)

// ServiceTargetPreview describes the changes and artifact work a service deployment would perform.
type ServiceTargetPreview struct {
	Target           ServiceTargetPreviewTarget    `json:"target"`
	Source           string                        `json:"source,omitempty"`
	Action           string                        `json:"action"`
	RemoteComparison string                        `json:"remoteComparison,omitempty"`
	RemoteVersion    string                        `json:"remoteVersion,omitempty"`
	Changes          []ServiceTargetPreviewChange  `json:"changes"`
	Artifact         *ServiceTargetPreviewArtifact `json:"artifact,omitempty"`
}

// ServiceTargetPreviewTarget identifies the resource represented by a deployment preview.
type ServiceTargetPreviewTarget struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ServiceTargetPreviewChange describes one property-level deployment change.
type ServiceTargetPreviewChange struct {
	Group  string `json:"group"`
	Field  string `json:"field"`
	Change string `json:"change"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// ServiceTargetPreviewArtifact describes artifact work that deployment would perform.
type ServiceTargetPreviewArtifact struct {
	Type        string `json:"type"`
	Reference   string `json:"reference,omitempty"`
	Digest      string `json:"digest,omitempty"`
	WouldBuild  bool   `json:"wouldBuild"`
	WouldPush   bool   `json:"wouldPush"`
	WouldUpload bool   `json:"wouldUpload"`
}

// MarkServiceTargetPreviewRequest marks a target resource used with Deploy as a preview request.
func MarkServiceTargetPreviewRequest(targetResource *TargetResource) {
	if targetResource == nil {
		return
	}
	if targetResource.Metadata == nil {
		targetResource.Metadata = map[string]string{}
	}
	targetResource.Metadata[ServiceTargetPreviewRequestMetadataKey] = "true"
}

// IsServiceTargetPreviewRequest reports whether a Deploy call requests read-only preview behavior.
func IsServiceTargetPreviewRequest(targetResource *TargetResource) bool {
	return targetResource != nil &&
		targetResource.Metadata[ServiceTargetPreviewRequestMetadataKey] == "true"
}

// SetServiceTargetPreviewEnvironment serializes the read-only environment snapshot for the provider.
func SetServiceTargetPreviewEnvironment(
	targetResource *TargetResource,
	environment map[string]string,
) error {
	if targetResource == nil {
		return errors.New("target resource is required")
	}
	payload, err := json.Marshal(environment)
	if err != nil {
		return fmt.Errorf("marshal service target preview environment: %w", err)
	}
	if targetResource.Metadata == nil {
		targetResource.Metadata = map[string]string{}
	}
	targetResource.Metadata[ServiceTargetPreviewEnvironmentMetadataKey] = string(payload)
	return nil
}

// ParseServiceTargetPreviewEnvironment reads the environment snapshot from a preview request.
func ParseServiceTargetPreviewEnvironment(targetResource *TargetResource) (map[string]string, error) {
	if !IsServiceTargetPreviewRequest(targetResource) {
		return nil, errors.New("service target preview request is required")
	}
	payload := targetResource.Metadata[ServiceTargetPreviewEnvironmentMetadataKey]
	if payload == "" {
		return nil, errors.New("service target preview environment not found")
	}
	var environment map[string]string
	if err := json.Unmarshal([]byte(payload), &environment); err != nil {
		return nil, fmt.Errorf("parse service target preview environment: %w", err)
	}
	return environment, nil
}

// NewServiceTargetPreviewResultArtifact serializes a preview for transport in a deploy response.
func NewServiceTargetPreviewResultArtifact(preview *ServiceTargetPreview) (*Artifact, error) {
	if preview == nil {
		return nil, errors.New("service target preview is required")
	}
	payload, err := json.Marshal(preview)
	if err != nil {
		return nil, fmt.Errorf("marshal service target preview: %w", err)
	}
	return &Artifact{
		Kind:         ArtifactKind_ARTIFACT_KIND_CONFIG,
		Location:     ServiceTargetPreviewArtifactLocation,
		LocationKind: LocationKind_LOCATION_KIND_REMOTE,
		Metadata: map[string]string{
			ServiceTargetPreviewResultMetadataKey: string(payload),
		},
	}, nil
}

// ParseServiceTargetPreviewResult reads a preview result from deploy response artifacts.
func ParseServiceTargetPreviewResult(artifacts []*Artifact) (*ServiceTargetPreview, error) {
	for _, artifact := range artifacts {
		if artifact == nil || artifact.Metadata == nil {
			continue
		}
		payload := artifact.Metadata[ServiceTargetPreviewResultMetadataKey]
		if payload == "" {
			continue
		}
		var preview ServiceTargetPreview
		if err := json.Unmarshal([]byte(payload), &preview); err != nil {
			return nil, fmt.Errorf("parse service target preview result: %w", err)
		}
		if strings.TrimSpace(preview.Target.Type) == "" ||
			strings.TrimSpace(preview.Target.Name) == "" ||
			strings.TrimSpace(preview.Action) == "" {
			return nil, errors.New("parse service target preview result: target type, target name, and action are required")
		}
		if preview.Changes == nil {
			preview.Changes = []ServiceTargetPreviewChange{}
		}
		return &preview, nil
	}
	return nil, errors.New("service target preview result not found")
}
