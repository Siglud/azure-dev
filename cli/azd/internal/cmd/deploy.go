// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/azure/azure-dev/cli/azd/cmd/actions"
	"github.com/azure/azure-dev/cli/azd/internal"
	"github.com/azure/azure-dev/cli/azd/pkg/account"
	"github.com/azure/azure-dev/cli/azd/pkg/alpha"
	"github.com/azure/azure-dev/cli/azd/pkg/apphost"
	"github.com/azure/azure-dev/cli/azd/pkg/azapi"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
	"github.com/azure/azure-dev/cli/azd/pkg/cloud"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/environment/azdcontext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/pkg/exegraph"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/ioc"
	"github.com/azure/azure-dev/cli/azd/pkg/lazy"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/azure/azure-dev/cli/azd/pkg/output/ux"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type DeployFlags struct {
	ServiceName string
	All         bool
	Preview     bool
	Timeout     int
	fromPackage string
	flagSet     *pflag.FlagSet
	global      *internal.GlobalCommandOptions
	*internal.EnvFlag
}

const defaultDeployTimeoutSeconds = 1200

func (d *DeployFlags) Bind(local *pflag.FlagSet, global *internal.GlobalCommandOptions) {
	d.BindNonCommon(local, global)
	d.bindCommon(local, global)
}

func (d *DeployFlags) BindNonCommon(
	local *pflag.FlagSet,
	global *internal.GlobalCommandOptions) {
	local.StringVar(
		&d.ServiceName,
		"service",
		"",
		//nolint:lll
		"Deploys a specific service (when the string is unspecified, all services that are listed in the "+azdcontext.ProjectFileName+" file are deployed).",
	)
	//deprecate:flag hide --service
	_ = local.MarkHidden("service")
	d.global = global
}

func (d *DeployFlags) bindCommon(local *pflag.FlagSet, global *internal.GlobalCommandOptions) {
	d.EnvFlag = &internal.EnvFlag{}
	d.EnvFlag.Bind(local, global)
	d.flagSet = local

	local.BoolVar(
		&d.All,
		"all",
		false,
		"Deploys all services that are listed in "+azdcontext.ProjectFileName,
	)
	local.BoolVar(
		&d.Preview,
		"preview",
		false,
		"Preview deployment changes without applying them (currently supported for Microsoft Foundry hosted agents).",
	)
	local.StringVar(
		&d.fromPackage,
		"from-package",
		"",
		//nolint:lll
		"Deploys the packaged service located at the provided path. Supports zipped file packages (file path) or container images (image tag).",
	)
	local.IntVar(
		&d.Timeout,
		"timeout",
		defaultDeployTimeoutSeconds,
		fmt.Sprintf(
			"Maximum time in seconds for azd to wait for each service deployment. This stops azd from waiting "+
				"but does not cancel the Azure-side deployment. (default: %d)",
			defaultDeployTimeoutSeconds,
		),
	)
}

func (d *DeployFlags) SetCommon(envFlag *internal.EnvFlag) {
	d.EnvFlag = envFlag
}

func NewDeployFlags(cmd *cobra.Command, global *internal.GlobalCommandOptions) *DeployFlags {
	flags := &DeployFlags{}
	flags.Bind(cmd.Flags(), global)

	return flags
}

func NewDeployFlagsFromEnvAndOptions(envFlag *internal.EnvFlag, global *internal.GlobalCommandOptions) *DeployFlags {
	return &DeployFlags{
		Timeout: defaultDeployTimeoutSeconds,
		EnvFlag: envFlag,
		global:  global,
	}
}

func (d *DeployFlags) timeoutChanged() bool {
	if d.flagSet == nil {
		return false
	}

	timeoutFlag := d.flagSet.Lookup("timeout")
	return timeoutFlag != nil && timeoutFlag.Changed
}

func (d *DeployFlags) fromPackageChanged() bool {
	return d.flagSet != nil && d.flagSet.Changed("from-package")
}

func NewDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <service>",
		Short: "Deploy your project code to Azure.",
	}
	cmd.Args = cobra.MaximumNArgs(1)

	return cmd
}

type DeployAction struct {
	flags               *DeployFlags
	args                []string
	projectConfig       *project.ProjectConfig
	azdCtx              *azdcontext.AzdContext
	env                 *environment.Environment
	envManager          environment.Manager
	projectManager      project.ProjectManager
	serviceManager      project.ServiceManager
	resourceManager     project.ResourceManager
	accountManager      account.Manager
	azCli               *azapi.AzureClient
	portalUrlBase       string
	formatter           output.Formatter
	writer              io.Writer
	console             input.Console
	commandRunner       exec.CommandRunner
	alphaFeatureManager *alpha.FeatureManager
	importManager       *project.ImportManager
	lazyServiceManager  *lazy.Lazy[project.ServiceManager]
	lazyResourceManager *lazy.Lazy[project.ResourceManager]
	serviceLocator      ioc.ServiceLocator
	progressTracker     *deployProgressTracker // set at runtime when using parallel deployment graph
}

func NewDeployAction(
	flags *DeployFlags,
	args []string,
	projectConfig *project.ProjectConfig,
	azdCtx *azdcontext.AzdContext,
	lazyServiceManager *lazy.Lazy[project.ServiceManager],
	lazyResourceManager *lazy.Lazy[project.ResourceManager],
	envManager environment.Manager,
	accountManager account.Manager,
	cloud *cloud.Cloud,
	azCli *azapi.AzureClient,
	commandRunner exec.CommandRunner,
	console input.Console,
	formatter output.Formatter,
	writer io.Writer,
	alphaFeatureManager *alpha.FeatureManager,
	importManager *project.ImportManager,
	serviceLocator ioc.ServiceLocator,
) actions.Action {
	return &DeployAction{
		flags:               flags,
		args:                args,
		projectConfig:       projectConfig,
		azdCtx:              azdCtx,
		lazyServiceManager:  lazyServiceManager,
		lazyResourceManager: lazyResourceManager,
		envManager:          envManager,
		accountManager:      accountManager,
		portalUrlBase:       cloud.PortalUrlBase,
		azCli:               azCli,
		formatter:           formatter,
		writer:              writer,
		console:             console,
		commandRunner:       commandRunner,
		alphaFeatureManager: alphaFeatureManager,
		importManager:       importManager,
		serviceLocator:      serviceLocator,
	}
}

type DeploymentResult struct {
	Timestamp time.Time                               `json:"timestamp"`
	Services  map[string]*project.ServiceDeployResult `json:"services"`
}

type DeploymentPreviewResult struct {
	Timestamp time.Time                               `json:"timestamp"`
	Services  map[string]*azdext.ServiceTargetPreview `json:"services"`
}

type deployPreviewTarget struct {
	service   *project.ServiceConfig
	previewer project.ServiceTargetPreviewer
}

func (da *DeployAction) Run(ctx context.Context) (*actions.ActionResult, error) {
	if da.flags.Preview {
		switch {
		case da.flags.fromPackageChanged():
			return nil, fmt.Errorf(
				"--from-package cannot be used with --preview: %w",
				internal.ErrInvalidFlagCombination,
			)
		case da.flags.timeoutChanged():
			return nil, fmt.Errorf(
				"--timeout cannot be used with --preview: %w",
				internal.ErrInvalidFlagCombination,
			)
		}
	}

	if err := da.loadEnvironment(ctx); err != nil {
		return nil, err
	}

	targetServiceName := da.flags.ServiceName
	if len(da.args) == 1 {
		targetServiceName = da.args[0]
	}

	if !da.flags.Preview && da.env.GetSubscriptionId() == "" {
		return nil, &internal.ErrorWithSuggestion{
			Err:        internal.ErrInfraNotProvisioned,
			Suggestion: "Run 'azd provision' to set up infrastructure before deploying.",
		}
	}

	var err error
	if da.flags.Preview {
		targetServiceName, err = da.getPreviewTargetServiceName(ctx, targetServiceName)
	} else {
		if err := da.loadManagers(); err != nil {
			return nil, err
		}
		targetServiceName, err = getTargetServiceName(
			ctx,
			da.projectManager,
			da.importManager,
			da.projectConfig,
			string(project.ServiceEventDeploy),
			targetServiceName,
			da.flags.All,
		)
	}
	if err != nil {
		return nil, err
	}

	if da.flags.All && da.flags.fromPackage != "" {
		return nil, &internal.ErrorWithSuggestion{
			Err:        internal.ErrFromPackageWithAll,
			Suggestion: "Use 'azd deploy <service> --from-package <path>' to target a specific service.",
		}
	}

	if targetServiceName == "" && da.flags.fromPackage != "" {
		return nil, &internal.ErrorWithSuggestion{
			Err:        internal.ErrFromPackageNoService,
			Suggestion: "Use 'azd deploy <service> --from-package <path>' to target a specific service.",
		}
	}

	stableServices, err := da.importManager.ServiceStableFiltered(
		ctx, da.projectConfig, targetServiceName, da.env.Getenv)
	if err != nil {
		return nil, err
	}

	if da.flags.Preview {
		previewTargets, err := da.resolvePreviewTargets(ctx, stableServices)
		if err != nil {
			return nil, err
		}
		for _, target := range previewTargets {
			if err := target.previewer.InitializePreview(ctx, target.service, da.env); err != nil {
				return nil, fmt.Errorf("initializing service target for %q: %w", target.service.Name, err)
			}
		}
		return da.previewServices(ctx, previewTargets)
	}

	if err := da.projectManager.InitializeServices(ctx, stableServices); err != nil {
		return nil, err
	}

	if err := da.projectManager.EnsureServiceTargetTools(ctx, stableServices); err != nil {
		return nil, err
	}

	// Command title
	da.console.MessageUxItem(ctx, &ux.MessageTitle{
		Title: "Deploying services (azd deploy)",
	})

	startTime := time.Now()

	// Always deploy through the service execution graph. The graph handles
	// any service count (including N=1) with a uniform progress tracker
	// and the same package → publish → deploy step topology.
	return da.deployServicesGraph(ctx, stableServices, startTime)
}

func (da *DeployAction) loadEnvironment(ctx context.Context) error {
	if da.env != nil {
		return nil
	}
	if da.flags.Preview {
		environmentName := da.flags.EnvironmentName
		if environmentName == "" {
			var err error
			environmentName, err = da.azdCtx.GetDefaultEnvironmentName()
			if err != nil {
				return &internal.ErrorWithSuggestion{
					Err:        fmt.Errorf("deployment preview requires an existing environment: %w", err),
					Suggestion: "Run 'azd env new <name>' before previewing deployment changes.",
				}
			}
		}
		if !isSafePreviewEnvironmentName(environmentName) {
			return &internal.ErrorWithSuggestion{
				Err: fmt.Errorf(
					"invalid environment name %q for deployment preview: %w",
					environmentName,
					internal.ErrInvalidFlagCombination,
				),
				Suggestion: "Use an existing environment name without path separators, '.' or '..'.",
			}
		}
		env, err := da.loadPreviewEnvironment(environmentName)
		if err != nil {
			return &internal.ErrorWithSuggestion{
				Err: fmt.Errorf(
					"deployment preview requires an existing environment: %w",
					err,
				),
				Suggestion: "Run 'azd env new <name>' before previewing deployment changes.",
			}
		}
		if name, found := env.LookupEnv(environment.EnvNameEnvVarName); !found || name != environmentName {
			env.DotenvSet(environment.EnvNameEnvVarName, environmentName)
		}
		da.env = env
		return nil
	}

	if err := da.serviceLocator.Resolve(&da.env); err != nil {
		return fmt.Errorf("loading environment: %w", err)
	}
	return nil
}

func (da *DeployAction) loadPreviewEnvironment(name string) (*environment.Environment, error) {
	root := da.azdCtx.EnvironmentRoot(name)
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, environment.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("inspect environment %q: %w", name, err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("environment %q is not a regular directory", name)
	}

	envPath := filepath.Join(root, environment.DotEnvFileName)
	envInfo, err := os.Lstat(envPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return environment.NewWithValues(name, map[string]string{}), nil
	case err != nil:
		return nil, fmt.Errorf("inspect environment values for %q: %w", name, err)
	case envInfo.Mode()&os.ModeSymlink != 0 || !envInfo.Mode().IsRegular():
		return nil, fmt.Errorf("environment values for %q are not a regular file", name)
	}

	values, err := godotenv.Read(envPath)
	if err != nil {
		return nil, fmt.Errorf("read environment values for %q: %w", name, err)
	}
	return environment.NewWithValues(name, values), nil
}

func isSafePreviewEnvironmentName(name string) bool {
	return environment.IsValidEnvironmentName(name) &&
		name != "." &&
		name != ".." &&
		!strings.ContainsAny(name, `/\`)
}

func (da *DeployAction) loadManagers() error {
	if da.projectManager != nil && da.serviceManager != nil &&
		da.lazyResourceManager == nil {
		return nil
	}
	if da.serviceManager == nil {
		serviceManager, err := da.lazyServiceManager.GetValue()
		if err != nil {
			return fmt.Errorf("loading service manager: %w", err)
		}
		da.serviceManager = serviceManager
	}
	if da.resourceManager == nil {
		resourceManager, err := da.lazyResourceManager.GetValue()
		if err != nil {
			return fmt.Errorf("loading resource manager: %w", err)
		}
		da.resourceManager = resourceManager
	}
	if da.projectManager == nil {
		da.projectManager = project.NewProjectManager(
			da.azdCtx,
			da.serviceManager,
			da.importManager,
		)
	}
	return nil
}

func (da *DeployAction) getPreviewTargetServiceName(
	ctx context.Context,
	targetServiceName string,
) (string, error) {
	if da.flags.All && targetServiceName != "" {
		return "", fmt.Errorf("cannot specify both --all and <service>")
	}
	if !da.flags.All && targetServiceName == "" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if workingDirectory != da.azdCtx.ProjectDirectory() {
			services, err := da.importManager.ServiceStable(ctx, da.projectConfig)
			if err != nil {
				return "", err
			}
			for _, service := range services {
				if workingDirectory == service.Path() {
					targetServiceName = service.Name
					break
				}
			}
			if targetServiceName == "" {
				return "", fmt.Errorf(
					"current working directory is not a project or service directory. " +
						"Specify a service name to deploy a service, or specify --all to deploy all services",
				)
			}
		}
	}
	if targetServiceName != "" {
		hasService, err := da.importManager.HasService(ctx, da.projectConfig, targetServiceName)
		if err != nil {
			return "", err
		}
		if !hasService {
			return "", fmt.Errorf("service name '%s' doesn't exist", targetServiceName)
		}
	}
	return targetServiceName, nil
}

func (da *DeployAction) resolvePreviewTargets(
	ctx context.Context,
	services []*project.ServiceConfig,
) ([]deployPreviewTarget, error) {
	targets := make([]deployPreviewTarget, 0, len(services))
	for _, service := range services {
		target, err := da.getPreviewServiceTarget(ctx, service)
		if err != nil {
			return nil, fmt.Errorf("getting service target for %q: %w", service.Name, err)
		}
		previewer, ok := target.(project.ServiceTargetPreviewer)
		if !ok || !previewer.SupportsPreview() {
			return nil, fmt.Errorf(
				"service %q uses target %q, which does not support deployment previews: %w",
				service.Name,
				service.Host,
				azapi.ErrPreviewNotSupported,
			)
		}
		targets = append(targets, deployPreviewTarget{
			service:   service,
			previewer: previewer,
		})
	}
	return targets, nil
}

func (da *DeployAction) getPreviewServiceTarget(
	ctx context.Context,
	service *project.ServiceConfig,
) (project.ServiceTarget, error) {
	if da.serviceManager != nil {
		return da.serviceManager.GetServiceTarget(ctx, service)
	}

	var target project.ServiceTarget
	if err := da.serviceLocator.ResolveNamed(string(service.Host), &target); err != nil {
		return nil, err
	}
	return target, nil
}

func (da *DeployAction) previewServices(
	ctx context.Context,
	targets []deployPreviewTarget,
) (*actions.ActionResult, error) {
	if da.formatter.Kind() != output.JsonFormat {
		da.console.MessageUxItem(ctx, &ux.MessageTitle{
			Title:     "Previewing service changes (azd deploy --preview)",
			TitleNote: "This is a preview. No deployment changes will be applied.",
		})
	}

	result := DeploymentPreviewResult{
		Timestamp: time.Now(),
		Services:  make(map[string]*azdext.ServiceTargetPreview, len(targets)),
	}
	for _, target := range targets {
		preview, err := target.previewer.Preview(ctx, target.service, da.env)
		if err != nil {
			return nil, fmt.Errorf("previewing service %q: %w", target.service.Name, err)
		}
		result.Services[target.service.Name] = preview
	}

	services := make([]*project.ServiceConfig, len(targets))
	for i, target := range targets {
		services[i] = target.service
	}
	if da.formatter.Kind() == output.JsonFormat {
		if err := da.formatter.Format(result, da.writer, nil); err != nil {
			return nil, fmt.Errorf("deployment preview could not be displayed: %w", err)
		}
	} else {
		displayServiceDeploymentPreviews(ctx, da.console, services, result.Services)
	}

	return &actions.ActionResult{
		Message: &actions.ResultMessage{
			Header: "Deployment preview completed. No changes were made.",
		},
	}, nil
}

func displayServiceDeploymentPreviews(
	ctx context.Context,
	console input.Console,
	services []*project.ServiceConfig,
	previews map[string]*azdext.ServiceTargetPreview,
) {
	for _, service := range services {
		preview := previews[service.Name]
		if preview == nil {
			continue
		}

		operation := ux.OperationTypeModify
		switch {
		case preview.Action == "create":
			operation = ux.OperationTypeCreate
		case preview.Action == "skip", preview.Action == "noChange":
			operation = ux.OperationTypeNoChange
		case len(preview.Changes) == 0:
			operation = ux.OperationTypeDeploy
		}

		deltas := make([]ux.PropertyDelta, 0, len(preview.Changes)+4)
		for _, change := range preview.Changes {
			changeType := "Modify"
			switch change.Change {
			case "add":
				changeType = "Create"
			case "remove":
				changeType = "Delete"
			}
			deltas = append(deltas, ux.PropertyDelta{
				Path:       change.Group + "." + change.Field,
				ChangeType: changeType,
				Before:     change.Before,
				After:      change.After,
			})
		}
		if artifact := preview.Artifact; artifact != nil {
			if preview.Action == "create" {
				deltas = append(deltas, ux.PropertyDelta{
					Path:       "artifact.type",
					ChangeType: "Create",
					After:      artifact.Type,
				})
			}
			if preview.Action == "create" && artifact.Reference != "" {
				deltas = append(deltas, ux.PropertyDelta{
					Path:       "artifact.reference",
					ChangeType: "Create",
					After:      artifact.Reference,
				})
			}
			for _, planned := range []struct {
				path  string
				value bool
			}{
				{path: "artifact.build", value: artifact.WouldBuild},
				{path: "artifact.push", value: artifact.WouldPush},
				{path: "artifact.upload", value: artifact.WouldUpload},
			} {
				if planned.value {
					deltas = append(deltas, ux.PropertyDelta{
						Path:       planned.path,
						ChangeType: "Create",
						After:      true,
					})
				}
			}
		}

		console.MessageUxItem(ctx, &ux.PreviewProvision{
			Operations: []*ux.Resource{{
				Operation:      operation,
				Type:           preview.Target.Type,
				Name:           preview.Target.Name,
				PropertyDeltas: deltas,
			}},
		})

		switch {
		case preview.RemoteComparison == "unavailableUntilProvision":
			console.Message(
				ctx,
				"  Remote comparison is unavailable until the target project is provisioned.",
			)
		case len(preview.Changes) == 0 && preview.Action == "createVersion":
			console.Message(
				ctx,
				"  No configuration changes. Deployment would still create a new immutable version.",
			)
		}
	}
}

// dotNetPackagePublishBuildGateKey groups standard .NET services whose
// package or publish phase can run dotnet publish locally.
func dotNetPackagePublishBuildGateKey(svc *project.ServiceConfig) string {
	if svc != nil && svc.DotNetContainerApp == nil && svc.Language.IsDotNet() {
		return "dotnet"
	}
	return ""
}

// deployServicesGraph builds an execution graph of service deployments and runs them in
// parallel via the execution graph scheduler. Concurrent local .NET publishes
// receive an isolated artifacts path when supported, with a shared runtime
// mutex as the compatibility fallback. The mutex is released before Azure
// deployment work begins.
func (da *DeployAction) deployServicesGraph(
	ctx context.Context,
	stableServices []*project.ServiceConfig,
	startTime time.Time,
) (*actions.ActionResult, error) {
	deployTimeout, err := da.resolveDeployTimeout()
	if err != nil {
		return nil, err
	}
	concurrency := resolveDeployGraphConcurrency(da.env.LookupEnv)

	// Wrap console for thread-safe output during parallel deployment.
	// Graph step callbacks may call ShowSpinner/StopSpinner/Message which are
	// not goroutine-safe on the underlying console.
	origConsole := da.console
	sc := &syncConsole{Console: origConsole}

	// Create a tracker and suppress individual service spinners so
	// the tracker owns the progress display. Skip the tracker entirely in
	// machine-readable output modes (e.g. --output json) so raw progress
	// lines don't pollute stdout alongside the JSON result, and when no
	// writer is available (e.g. test mocks).
	if w := origConsole.GetWriter(); da.formatter.Kind() != output.JsonFormat && w != nil {
		serviceNames := make([]string, len(stableServices))
		for i, svc := range stableServices {
			serviceNames[i] = svc.Name
		}
		da.progressTracker = newDeployProgressTracker(
			w,
			origConsole.IsSpinnerInteractive(),
			serviceNames,
		)
		da.console = &silentSpinnerConsole{syncConsole: sc}
		// Suppress previewer output at the shared console level so that
		// DI-injected consumers (e.g. ContainerHelper's Docker output)
		// don't corrupt the progress table display.
		if ps, ok := origConsole.(input.PreviewerPauser); ok {
			ps.PausePreviewer()
			defer ps.ResumePreviewer()
		}
	} else {
		// Still wrap the console for thread-safety without suppressing spinners.
		da.console = sc
	}
	defer func() {
		da.console = origConsole
		da.progressTracker = nil
	}()

	g := exegraph.NewGraph()
	state := newDeployGraphState(stableServices)
	var packagePublishBuildGateKey func(*project.ServiceConfig) string
	if da.flags.fromPackage == "" {
		packagePublishBuildGateKey = dotNetPackagePublishBuildGateKey
	}

	if _, err := addServiceStepsToGraph(g, serviceGraphOptions{
		services:                   stableServices,
		serviceManager:             da.serviceManager,
		deployTimeout:              deployTimeout,
		maxConcurrency:             concurrency.max,
		fromPackage:                da.flags.fromPackage,
		state:                      state,
		packagePublishBuildGateKey: packagePublishBuildGateKey,
		buildGateKey:               aspireBuildGateKey,
		onDeployTimeout: func(ctx context.Context, svc *project.ServiceConfig) {
			da.console.MessageUxItem(ctx, deployTimeoutWarning(svc.Name, deployTimeout))
		},
		onPhaseProgress: func(svcName string, phase deployPhase, detail string) {
			// Forward intra-phase progress (e.g. "Pushing image…") to the
			// tracker so the table's Detail column reflects what each step
			// is doing — not just which phase. updateProgress is a no-op
			// when the tracker is nil (JSON output / no writer paths).
			da.updateProgress(svcName, phase, detail)
		},
	}); err != nil {
		return nil, err
	}

	// Wire progress tracker to graph step lifecycle callbacks.
	// Step names are "package-<svc>", "publish-<svc>", "deploy-<svc>".
	opts := exegraph.RunOptions{
		MaxConcurrency:   concurrency.max,
		GroupConcurrency: concurrency.groups,
		ErrorPolicy:      exegraph.FailFast,
		OnStepStart: func(stepName string) {
			if svc, ok := strings.CutPrefix(stepName, "package-"); ok {
				da.updateProgress(svc, phasePackaging, "")
			} else if svc, ok := strings.CutPrefix(stepName, "publish-"); ok {
				da.updateProgress(svc, phasePublish, "")
			} else if svc, ok := strings.CutPrefix(stepName, "deploy-"); ok {
				da.updateProgress(svc, phaseDeploying, "")
			}
		},
		OnStepDone: func(stepName string, err error) {
			if err != nil {
				// Classify terminal state: skipped (dependency failure or
				// FailFast cascade) and parent-cancellation both surface via
				// OnStepDone with a non-nil error, but they are not service
				// failures and should not render as "Failed" in the progress
				// UI.
				phase := phaseFailed
				detail := err.Error()
				switch {
				case exegraph.IsStepSkipped(err):
					phase = phaseSkipped
					detail = ""
				case errors.Is(err, context.Canceled):
					phase = phaseSkipped
					detail = "canceled"
				}
				for _, prefix := range []string{"deploy-", "publish-", "package-"} {
					if svc, ok := strings.CutPrefix(stepName, prefix); ok {
						da.updateProgress(svc, phase, detail)
						return
					}
				}
			}
			if svc, ok := strings.CutPrefix(stepName, "deploy-"); ok {
				da.updateProgress(svc, phaseDone, "")
			}
		},
	}

	projectEventArgs := project.ProjectLifecycleEventArgs{
		Project: da.projectConfig,
	}

	// Start the progress ticker if the tracker is active.
	var stopTicker func()
	if da.progressTracker != nil {
		stopTicker = da.progressTracker.StartTicker(ctx)
	}

	err = da.projectConfig.Invoke(ctx, project.ProjectEventDeploy, projectEventArgs, func() error {
		result := exegraph.RunWithResult(ctx, g, opts)
		// Log per-step timing for diagnostics and benchmarking.
		for _, st := range result.Steps {
			log.Printf("deploy-graph step %-30s  %s  %s", st.Name, st.Status, st.Duration.Round(time.Millisecond))
		}
		log.Printf("deploy-graph total: %s (%d steps)", result.TotalDuration.Round(time.Millisecond), len(result.Steps))

		// Unwrap the graph runner's step-level error prefix ("step X failed: ...")
		// so user-facing messages contain only the action error, not internal graph
		// framing. When exactly one step failed, return its inner error directly;
		// when multiple failed, join their inner errors.
		return unwrapStepErrors(result)
	})

	// Stop ticker and render final progress state.
	if stopTicker != nil {
		stopTicker()
	}
	if da.progressTracker != nil {
		da.progressTracker.RenderFinal()
	}

	// Clean up temporary package artifacts created during graph execution.
	if da.flags.fromPackage == "" {
		state.CleanupTempArtifacts()
	}

	if err != nil {
		return nil, err
	}

	if da.formatter.Kind() != output.JsonFormat {
		displayDeployWarnings(ctx, da.console, stableServices, state)
	}

	// Display service endpoint artifacts collected during deploy steps.
	if da.formatter.Kind() != output.JsonFormat {
		for _, svc := range stableServices {
			if dr := state.GetResult(svc.Name); dr != nil && len(dr.Artifacts) > 0 {
				da.console.MessageUxItem(ctx, dr.Artifacts)
			}
		}
	}

	aspireDashboardUrl := apphost.AspireDashboardUrl(ctx, da.env, da.alphaFeatureManager)
	if aspireDashboardUrl != nil {
		da.console.MessageUxItem(ctx, aspireDashboardUrl)
	}

	if da.formatter.Kind() == output.JsonFormat {
		deployResult := DeploymentResult{
			Timestamp: time.Now(),
			Services:  state.ResultsSnapshot(),
		}

		if fmtErr := da.formatter.Format(deployResult, da.writer, nil); fmtErr != nil {
			return nil, fmt.Errorf("deploy result could not be displayed: %w", fmtErr)
		}
	}

	// Invalidate cache after successful deploy so azd show will refresh
	if err := da.envManager.InvalidateEnvCache(ctx, da.env.Name()); err != nil {
		log.Printf("warning: failed to invalidate state cache: %v", err)
	}

	return &actions.ActionResult{
		Message: &actions.ResultMessage{
			Header: fmt.Sprintf(
				"Your application was deployed to Azure in %s.",
				ux.DurationAsText(since(startTime)),
			),
			FollowUp: getResourceGroupFollowUp(ctx,
				da.formatter,
				da.portalUrlBase,
				da.projectConfig,
				da.resourceManager,
				da.env,
				false,
			),
		},
	}, nil
}

func (da *DeployAction) resolveDeployTimeout() (time.Duration, error) {
	return resolveDeployTimeout(da.flags)
}

// resolveDeployTimeout picks the deploy-per-service timeout from, in order:
//
//  1. --timeout CLI flag (if set by the user)
//  2. AZD_DEPLOY_TIMEOUT environment variable (integer seconds)
//  3. [defaultDeployTimeoutSeconds]
//
// Exposed as a free function so [UpGraphAction] — which shares the same
// [DeployFlags] type but not the [DeployAction] receiver — can resolve the
// timeout without duplicating the precedence logic.
func resolveDeployTimeout(flags *DeployFlags) (time.Duration, error) {
	if flags != nil && flags.timeoutChanged() {
		if flags.Timeout <= 0 {
			return 0, errors.New("invalid value for --timeout: must be greater than 0 seconds")
		}

		return time.Duration(flags.Timeout) * time.Second, nil
	}

	if envVal, ok := os.LookupEnv("AZD_DEPLOY_TIMEOUT"); ok {
		seconds, err := strconv.Atoi(envVal)
		if err != nil {
			return 0, fmt.Errorf("invalid AZD_DEPLOY_TIMEOUT value '%s': must be an integer number of seconds", envVal)
		}
		if seconds <= 0 {
			return 0, fmt.Errorf("invalid AZD_DEPLOY_TIMEOUT value '%d': must be greater than 0 seconds", seconds)
		}
		return time.Duration(seconds) * time.Second, nil
	}

	return time.Duration(defaultDeployTimeoutSeconds) * time.Second, nil
}

func GetCmdDeployHelpDescription(*cobra.Command) string {
	return generateCmdHelpDescription("Deploy application to Azure.", []string{
		formatHelpNote(
			"By default, deploys all services listed in 'azure.yaml' in the current directory," +
				" or the service described in the project that matches the current directory."),
		formatHelpNote(
			fmt.Sprintf("When %s is set, only the specific service is deployed.", output.WithHighLightFormat("<service>"))),
		formatHelpNote(
			fmt.Sprintf(
				"Use %s to inspect changes without packaging, publishing, or deploying.",
				output.WithHighLightFormat("--preview"),
			)),
		formatHelpNote("After the deployment is complete, the endpoint is printed. To start the service, select" +
			" the endpoint or paste it in a browser."),
	})
}

func GetCmdDeployHelpFooter(*cobra.Command) string {
	return generateCmdHelpSamplesBlock(map[string]string{
		"Deploy all services in the current project to Azure.": output.WithHighLightFormat(
			"azd deploy --all",
		),
		"Deploy the service named 'api' to Azure.": output.WithHighLightFormat(
			"azd deploy api",
		),
		"Deploy the service named 'web' to Azure.": output.WithHighLightFormat(
			"azd deploy web",
		),
		"Deploy the service named 'api' to Azure from a previously generated package.": output.WithHighLightFormat(
			"azd deploy api --from-package <package-path>",
		),
		"Preview changes to the service named 'agent' without deploying.": output.WithHighLightFormat(
			"azd deploy agent --preview",
		),
	})
}

// updateProgress notifies the progress tracker if it is active.
// When the tracker is nil (single-service path), this is a no-op.
func (da *DeployAction) updateProgress(serviceName string, phase deployPhase, detail string) {
	if da.progressTracker != nil {
		da.progressTracker.Update(serviceName, phase, detail)
	}
}

// unwrapStepErrors extracts the inner (action-level) errors from a graph
// RunResult, stripping the graph scheduler's "step %q failed: " prefix added
// by runStep. This keeps user-facing deploy errors clean — the step-name
// framing is useful for diagnostics logs but should not leak to users.
//
// Skipped steps (dependency failures) are omitted — only genuine step Action
// errors are returned. When exactly one step failed, its inner error is
// returned directly (not wrapped in errors.Join).
func unwrapStepErrors(result *exegraph.RunResult) error {
	if result.Error == nil {
		return nil
	}

	var inner []error
	for _, st := range result.Steps {
		if st.Err == nil || st.Status == exegraph.StepSkipped {
			continue
		}
		// runStep wraps with fmt.Errorf("step %q failed: %w", ...) — one Unwrap
		// level peels off that prefix while preserving the action error chain.
		if unwrapped := errors.Unwrap(st.Err); unwrapped != nil {
			inner = append(inner, unwrapped)
		} else {
			inner = append(inner, st.Err)
		}
	}

	switch len(inner) {
	case 0:
		// Shouldn't happen if result.Error != nil, but be safe.
		return result.Error
	case 1:
		return inner[0]
	default:
		return errors.Join(inner...)
	}
}

// silentSpinnerConsole wraps syncConsole but suppresses spinner output.
// When the progress table is active, the tracker owns the progress display
// and per-service spinners would interfere with the table rendering.
//
// Dropped spinner calls are logged at debug level so extension authors can
// diagnose missing spinner output from custom service targets — the swallow
// is intentional but invisible by default, and a silent no-op makes
// "my spinner doesn't show up" hard to root-cause without log access.
type silentSpinnerConsole struct {
	*syncConsole
}

func (*silentSpinnerConsole) ShowSpinner(_ context.Context, title string, _ input.SpinnerUxType) {
	log.Printf("silentSpinnerConsole: dropped ShowSpinner(%q) — progress table active", title)
}

func (*silentSpinnerConsole) StopSpinner(_ context.Context, title string, _ input.SpinnerUxType) {
	log.Printf("silentSpinnerConsole: dropped StopSpinner(%q) — progress table active", title)
}

func (*silentSpinnerConsole) IsSpinnerRunning(_ context.Context) bool { return false }

func (*silentSpinnerConsole) ShowPreviewer(_ context.Context, _ *input.ShowPreviewerOptions) io.Writer {
	log.Printf("silentSpinnerConsole: dropped ShowPreviewer — progress table active")
	return io.Discard
}

func (*silentSpinnerConsole) StopPreviewer(_ context.Context, _ bool) {
	log.Printf("silentSpinnerConsole: dropped StopPreviewer — progress table active")
}
