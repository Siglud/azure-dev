# Extension Framework

Architecture of the gRPC-based extension system in azd.

## Overview

Extensions are external processes that communicate with azd via gRPC. They allow third parties and first-party teams to add new capabilities — languages, hosting targets, event handlers, and more — without modifying the core CLI.

## Architecture

```text
azd (host)
  ├── Extension Registry (discovery)
  ├── Extension Manager (lifecycle)
  └── gRPC Broker (communication)
        ↕ gRPC
      Extension Process
        ├── Capability Handlers
        └── Service Implementations
```

### Discovery

Extensions are discovered from registries — JSON manifests that list available extensions with their versions, capabilities, and download URLs.

- **Official registry:** `https://aka.ms/azd/extensions/registry` — Stable, signed, production-ready extensions vetted by the azd team.
- **Dev registry:** `https://aka.ms/azd/extensions/registry/dev` — Experimental and pre-release extensions (unsigned builds, backed by `cli/azd/extensions/registry.dev.json`). Not configured by default; users opt in with `azd extension source add`.
- **Local sources:** File-based manifests for development

The dev registry serves as a staging area for extensions before they graduate to the main registry. Extensions installed from the dev registry are automatically promoted to the main registry when a newer stable version becomes available there. See the [Extension Resolution and Versioning](../../cli/azd/docs/extensions/extension-resolution-and-versioning.md#devexperimental-extension-registry) guide for detailed criteria, stability expectations, and submission guidelines.

### Lifecycle

1. User installs an extension: `azd extension install <name>`
2. Extension binary is downloaded and cached locally
3. When needed, azd spawns the extension process
4. gRPC connection is established via the broker
5. azd invokes capability methods on the extension
6. Extension responds via gRPC

### Communication

The gRPC broker (`pkg/grpcbroker`) manages bidirectional communication. Extensions can both:

- **Receive calls** from azd (e.g., "build this service")
- **Make calls** back to azd (e.g., "prompt the user", "read environment config")

## Capabilities

Extensions declare their capabilities in `extension.yaml`:

| Capability | Description |
|---|---|
| `custom-commands` | Expose new command groups and commands to azd |
| `lifecycle-events` | Subscribe to azd project and service lifecycle events (pre/post provision, deploy, etc.) |
| `mcp-server` | Provide Model Context Protocol tools for AI agents |
| `framework-service-provider` | Add build/restore support for new languages |
| `service-target-provider` | Add deployment support for new hosting targets |
| `provisioning-provider` | Add custom infrastructure provisioning support |
| `metadata` | Provide metadata about commands and capabilities |

## Available gRPC Services

Extensions can access these azd services via gRPC:

- **Project** — Read project configuration
- **Environment** — Read/write environment values and secrets
- **User Config** — Read user-level azd configuration
- **Deployment** — Access deployment information
- **Account**. List subscriptions and resolve access tenants. The `v1beta` client also retrieves the current principal's resource-tenant object ID and type for role assignments. See [GetCurrentPrincipal](../../cli/azd/docs/extensions/extension-framework.md#getcurrentprincipal).
- **Prompt** — Display prompts and collect user input
- **AI Model** — Query AI model availability and quotas
- **Event** — Subscribe to and emit events
- **Container** — Container registry operations
- **Framework** — Framework service operations
- **Service Target** — Deployment target operations

## Error Handling

Extensions use two structured error types:

- **`ServiceError`** — For Azure API or remote service errors
- **`LocalError`** — For client-side validation or configuration errors

Error precedence: ServiceError → LocalError → azcore.ResponseError → gRPC auth → fallback

## Deployment Preview SDK Contract

The SDK defines optional preview support through `WithServiceTargetPreview` and
`ServiceTargetPreviewProvider`. Registration advertises the capability without
constructing providers. Existing registrations default to no preview support.

Dedicated protocol messages carry a service configuration and return a
human-readable message plus structured data. The SDK handles each preview on a
fresh provider without invoking deployment initialization or using its instance
cache; preview is not dispatched as a deployment.

`azd deploy --preview` checks the advertised capability and sends only a preview
request. It retains normal authentication, but bypasses deployment hooks, generated
service imports, framework initialization, packaging, and deployment. Environment
reads use detached snapshots without local hydration, normalization writes, or
lock-file creation. Environment mutation requests from the extension are rejected.

The first-party implementation supports hosted Foundry agents (`azure.ai.agent`)
defined in the unified service-level `azure.yaml` format only. Legacy agent files
and deprecated nested `config:` definitions are unsupported for preview; ordinary
deployment remains unchanged. Unused legacy files do not override a modern service.
Build/code artifacts that cannot be determined without building or uploading are
reported as unknown, not as unchanged. See the
[agents extension guide](../../cli/azd/extensions/azure.ai.agents/README.md).

This implementation is draft work dependent on
[Azure/azure-dev#10055](https://github.com/Azure/azure-dev/pull/10055) merging and
an SDK release containing its contracts. It is not available in a released CLI
or extension merely because the SDK prerequisite exists.
See the [SDK contract](../../cli/azd/docs/extensions/extension-framework.md#deployment-preview-sdk-contract)
for registration and provider requirements.

## First-Party Extensions

First-party extensions live in `cli/azd/extensions/` and are registered in `cli/azd/extensions/registry.json`.

## Detailed Reference

- [Extension Framework Guide](../../cli/azd/docs/extensions/extension-framework.md) — Getting started
- [Extension Framework Services](../../cli/azd/docs/extensions/extension-framework-services.md) — Adding language support
- [Extensions Style Guide](../../cli/azd/docs/extensions/extensions-style-guide.md) — Design guidelines
- [Creating an Extension](../guides/creating-an-extension.md) — Step-by-step guide
