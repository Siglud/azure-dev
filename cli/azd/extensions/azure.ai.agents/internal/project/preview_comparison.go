// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"azureaiagent/internal/pkg/agents/agent_api"

	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"google.golang.org/protobuf/types/known/structpb"
)

type previewChange struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

type agentPreviewResult struct {
	Service string          `json:"service"`
	Agent   string          `json:"agent"`
	Status  string          `json:"status"`
	Changes []previewChange `json:"changes"`
	Unknown []string        `json:"unknown"`
	Notes   []string        `json:"notes"`
}

func comparePreviewRequest(
	service string, desired *agent_api.CreateAgentRequest, existing *agent_api.AgentObject, unknown []string,
) (*azdext.ServiceDeployPreviewResult, error) {
	after, secrets, err := previewRequestState(desired)
	if err != nil {
		return nil, previewConfigurationError()
	}
	before := map[string]any{}
	if existing != nil {
		latest := existing.Versions.Latest
		remote := &agent_api.CreateAgentRequest{
			Name: existing.Name,
			CreateAgentVersionRequest: agent_api.CreateAgentVersionRequest{
				Description: latest.Description, Metadata: latest.Metadata, Definition: latest.Definition,
				DigitalWorkerType: existing.DigitalWorkerType,
			},
		}
		// These properties are patched only when explicitly authored.
		if desired.AgentEndpoint != nil {
			remote.AgentEndpoint = existing.AgentEndpoint
		}
		if desired.AgentCard != nil {
			remote.AgentCard = existing.AgentCard
		}
		var remoteSecrets []string
		before, remoteSecrets, err = previewRequestState(remote)
		if err != nil {
			return nil, fmt.Errorf("Foundry returned an invalid hosted-agent definition; preview cannot compare it")
		}
		secrets = append(secrets, remoteSecrets...)
	}
	clean := func(value string) string {
		value = redactPreviewURLs(value)
		for _, secret := range secrets {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[redacted]")
			}
		}
		return value
	}
	result := agentPreviewResult{
		Service: clean(service), Agent: clean(desired.Name),
		Status: "noChange", Changes: []previewChange{}, Unknown: []string{},
		Notes: []string{
			"Read-only comparison of the latest agent version; infrastructure and dependencies are not previewed.",
			"Values are omitted from changes to protect environment values and credentials.",
		},
	}
	keys := maps.Clone(before)
	maps.Copy(keys, after)
	for _, path := range slices.Sorted(maps.Keys(keys)) {
		if slices.Contains(unknown, path) {
			continue
		}
		oldValue, oldExists := before[path]
		newValue, newExists := after[path]
		if !newExists && (strings.HasPrefix(path, "agent_endpoint.") || strings.HasPrefix(path, "agent_card.")) {
			// PATCH preserves unmentioned properties; these are not version
			// definition fields that a new version replaces.
			continue
		}
		if oldExists == newExists && reflect.DeepEqual(oldValue, newValue) {
			continue
		}
		operation := "update"
		if !oldExists {
			operation = "add"
		} else if !newExists {
			operation = "remove"
		}
		result.Changes = append(result.Changes, previewChange{Path: clean(path), Operation: operation})
	}
	for _, path := range unknown {
		result.Unknown = append(result.Unknown, clean(path))
	}
	slices.Sort(result.Unknown)
	result.Unknown = slices.Compact(result.Unknown)
	switch {
	case existing == nil:
		result.Status = "create"
	case len(result.Changes) > 0:
		result.Status = "update"
	case len(result.Unknown) > 0:
		result.Status = "unknown"
	}
	if len(result.Unknown) > 0 {
		result.Notes = append(result.Notes,
			"Unknown fields require build/upload or unresolved inputs; preview does not build, push, or upload artifacts.")
	}
	return previewResponse(result)
}

func previewRequestState(request *agent_api.CreateAgentRequest) (map[string]any, []string, error) {
	data, err := json.Marshal(request.Definition)
	if err != nil {
		return nil, nil, err
	}
	var definition agent_api.HostedAgentDefinition
	if err := json.Unmarshal(data, &definition); err != nil {
		return nil, nil, err
	}
	if definition.Kind != agent_api.AgentKindHosted || definition.CPU == "" || definition.Memory == "" ||
		(definition.ContainerConfiguration == nil && definition.CodeConfiguration == nil) {
		return nil, nil, fmt.Errorf("invalid hosted definition")
	}
	if definition.CodeConfiguration != nil {
		if definition.CodeConfiguration.Runtime == "" || len(definition.CodeConfiguration.EntryPoint) == 0 {
			return nil, nil, fmt.Errorf("invalid code configuration")
		}
	} else if definition.ContainerConfiguration.Image == "" {
		return nil, nil, fmt.Errorf("invalid container configuration")
	}
	if definition.SessionConfiguration == nil {
		definition.SessionConfiguration = &agent_api.SessionConfigurationAPI{IdleTimeoutSeconds: 900}
	}
	if cpu, err := strconv.ParseFloat(definition.CPU, 64); err == nil {
		definition.CPU = strconv.FormatFloat(cpu, 'f', -1, 64)
	}
	slices.SortFunc(definition.ProtocolVersions, func(a, b agent_api.ProtocolVersionRecord) int {
		return strings.Compare(string(a.Protocol)+"/"+a.Version, string(b.Protocol)+"/"+b.Version)
	})
	var secrets []string
	for _, value := range definition.EnvironmentVariables {
		secrets = append(secrets, value)
	}
	clone := *request
	clone.Definition = definition
	data, err = json.Marshal(clone)
	if err != nil {
		return nil, nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, nil, err
	}
	result := map[string]any{}
	flattenPreviewState("", object, result)
	return result, secrets, nil
}

func flattenPreviewState(prefix string, object map[string]any, result map[string]any) {
	for key, value := range object {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if nested, ok := value.(map[string]any); ok {
			flattenPreviewState(path, nested, result)
		} else {
			result[path] = value
		}
	}
}

var previewURLPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

func redactPreviewURLs(value string) string {
	return previewURLPattern.ReplaceAllStringFunc(value, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "[redacted URL]"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	})
}

func previewResponse(result agentPreviewResult) (*azdext.ServiceDeployPreviewResult, error) {
	var message bytes.Buffer
	if err := writeAgentPreview(&message, result); err != nil {
		return nil, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("cannot encode deployment preview")
	}
	var values map[string]any
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("cannot encode deployment preview data")
	}
	fields, err := structpb.NewStruct(values)
	if err != nil {
		return nil, fmt.Errorf("cannot encode deployment preview fields")
	}
	return &azdext.ServiceDeployPreviewResult{Message: message.String(), Data: fields}, nil
}

func writeAgentPreview(writer io.Writer, result agentPreviewResult) error {
	var message strings.Builder
	switch result.Status {
	case "create":
		message.WriteString("Would create the hosted agent.\n")
	case "update":
		message.WriteString("Would update the hosted agent.\n")
	case "unknown":
		message.WriteString("Known configuration matches; artifact or input changes are unknown.\n")
	default:
		message.WriteString("No deployment configuration changes.\n")
	}
	for _, change := range result.Changes {
		fmt.Fprintf(&message, "  %s: %s\n", change.Operation, change.Path)
	}
	for _, unknown := range result.Unknown {
		fmt.Fprintf(&message, "  unknown: %s\n", unknown)
	}
	for _, note := range result.Notes {
		fmt.Fprintln(&message, note)
	}
	_, err := io.WriteString(writer, message.String())
	return err
}
