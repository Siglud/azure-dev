# Feature Status

Current maturity status of Azure Developer CLI features. See [Feature Stages](../concepts/feature-stages.md) for what each stage means.

## Commands

| Feature | Stage |
|---|---|
| `add` | Beta |
| `auth` | Stable |
| `config` | Stable |
| `deploy` | Stable |
| `deploy --preview` (unified hosted Foundry agents only) | In development; not released |
| `down` | Stable |
| `env` | Stable |
| `help` | Stable |
| `infra generate` | Beta |
| `init` | Stable |
| `monitor` | Beta |
| `package` | Beta |
| `pipeline` | Beta |
| `provision` | Stable |
| `restore` | Beta |
| `show` | Stable |
| `template` | Beta |
| `up` | Stable |
| `update` | Beta |
| `version` | Stable |

Deployment preview is draft work associated with
[Azure/azure-dev#8549](https://github.com/Azure/azure-dev/issues/8549), blocked on the
[SDK prerequisite](https://github.com/Azure/azure-dev/pull/10055) merging and being
published. It supports unified service-level `azure.yaml` definitions for
`host: azure.ai.agent` only. Legacy `agent.yaml`/`agent.yml` and deprecated nested
`config:` definitions return an unsupported error; they continue to work with
ordinary deployment. See the [agents extension guide](../../cli/azd/extensions/azure.ai.agents/README.md)
for migration guidance and artifact-comparison limitations.

## Languages

| Language | Stage |
|---|---|
| Python | Stable |
| JavaScript / TypeScript | Stable |
| Java | Stable |
| .NET (C#) | Stable |

## Infrastructure as Code

| Provider | Stage |
|---|---|
| Bicep | Stable |
| Terraform | Beta |

## Hosting Targets

| Host | Stage |
|---|---|
| Azure App Service | Stable |
| Azure Static Web Apps | Stable |
| Azure Container Apps | Stable |
| Azure Functions | Stable |
| Azure Kubernetes Service (AKS) | Beta |
| Azure AI | Beta |

## Clients

| Client | Stage |
|---|---|
| VS Code Extension | Beta |
| Codespaces | Beta |
| Cloud Shell | Beta |
| Visual Studio | Alpha |

## CI/CD

| Platform | Stage |
|---|---|
| GitHub Actions | Stable |
| Azure Pipelines | Stable |

## Advanced Features

| Feature | Stage |
|---|---|
| Resource Group Deployments | Beta |
| Layered Provisioning | Beta |
| Deployment Stacks | Alpha |
| Extensions | Alpha |

---

For the full feature tracking table, see [cli/azd/docs/feature-stages.md](../../cli/azd/docs/feature-stages.md).
