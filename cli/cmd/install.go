package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/golang/protobuf/ptypes"
	pb "github.com/linkerd/linkerd2/controller/gen/config"
	"github.com/linkerd/linkerd2/pkg/charts"
	l5dcharts "github.com/linkerd/linkerd2/pkg/charts/linkerd2"
	"github.com/linkerd/linkerd2/pkg/config"
	"github.com/linkerd/linkerd2/pkg/healthcheck"
	"github.com/linkerd/linkerd2/pkg/issuercerts"
	"github.com/linkerd/linkerd2/pkg/k8s"
	consts "github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/tls"
	"github.com/linkerd/linkerd2/pkg/version"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/helm/pkg/chartutil"
	"sigs.k8s.io/yaml"
)

type (
	// installOptions holds values for command line flags that apply to the install
	// command. All fields in this struct should have corresponding flags added in
	// the newCmdInstall func later in this file. It also embeds proxyConfigOptions
	// in order to hold values for command line flags that apply to both inject and
	// install.
	installOptions struct {
		clusterDomain               string
		addOnConfig                 string
		controlPlaneVersion         string
		controllerReplicas          uint
		controllerLogLevel          string
		prometheusImage             string
		highAvailability            bool
		controllerUID               int64
		disableH2Upgrade            bool
		disableHeartbeat            bool
		cniEnabled                  bool
		skipChecks                  bool
		omitWebhookSideEffects      bool
		restrictDashboardPrivileges bool
		controlPlaneTracing         bool
		identityOptions             *installIdentityOptions
		smiMetricsEnabled           bool
		smiMetricsImage             string
		*proxyConfigOptions

		recordedFlags []*pb.Install_Flag

		// function pointers that can be overridden for tests
		heartbeatSchedule func() string
	}

	installIdentityOptions struct {
		replicas    uint
		trustDomain string

		issuanceLifetime   time.Duration
		clockSkewAllowance time.Duration

		trustPEMFile, crtPEMFile, keyPEMFile string
		identityExternalIssuer               bool
	}

	// helper struct to move those values together
	identityWithAnchorsAndTrustDomain struct {
		Identity        *l5dcharts.Identity
		TrustAnchorsPEM string
		TrustDomain     string
	}
)

const (

	// addOnChartsPath is where the linkerd2 add-ons will be present
	addOnChartsPath = "add-ons"

	configStage       = "config"
	controlPlaneStage = "control-plane"

	defaultIdentityIssuanceLifetime   = 24 * time.Hour
	defaultIdentityClockSkewAllowance = 20 * time.Second

	helmDefaultChartName = "linkerd2"
	helmDefaultChartDir  = "linkerd2"

	errMsgCannotInitializeClient = `Unable to install the Linkerd control plane. Cannot connect to the Kubernetes cluster:

%s

You can use the --ignore-cluster flag if you just want to generate the installation config.`

	errMsgGlobalResourcesExist = `Unable to install the Linkerd control plane. It appears that there is an existing installation:

%s

If you are sure you'd like to have a fresh install, remove these resources with:

    linkerd install --ignore-cluster | kubectl delete -f -

Otherwise, you can use the --ignore-cluster flag to overwrite the existing global resources.
`

	errMsgLinkerdConfigResourceConflict = "Can't install the Linkerd control plane in the '%s' namespace. Reason: %s.\nIf this is expected, use the --ignore-cluster flag to continue the installation.\n"
	errMsgGlobalResourcesMissing        = "Can't install the Linkerd control plane in the '%s' namespace. The required Linkerd global resources are missing.\nIf this is expected, use the --skip-checks flag to continue the installation.\n"
)

var (
	templatesConfigStage = []string{
		"templates/namespace.yaml",
		"templates/identity-rbac.yaml",
		"templates/controller-rbac.yaml",
		"templates/destination-rbac.yaml",
		"templates/heartbeat-rbac.yaml",
		"templates/web-rbac.yaml",
		"templates/serviceprofile-crd.yaml",
		"templates/trafficsplit-crd.yaml",
		"templates/prometheus-rbac.yaml",
		"templates/proxy-injector-rbac.yaml",
		"templates/sp-validator-rbac.yaml",
		"templates/tap-rbac.yaml",
		"templates/psp.yaml",
		"templates/smi-metrics-rbac.yaml",
	}

	templatesControlPlaneStage = []string{
		"templates/_config.tpl",
		"templates/_helpers.tpl",
		"templates/config.yaml",
		"templates/identity.yaml",
		"templates/controller.yaml",
		"templates/destination.yaml",
		"templates/heartbeat.yaml",
		"templates/web.yaml",
		"templates/prometheus.yaml",
		"templates/proxy-injector.yaml",
		"templates/sp-validator.yaml",
		"templates/tap.yaml",
		"templates/smi-metrics.yaml",
		"templates/linkerd-config-addons.yaml",
	}
)

// newInstallOptionsWithDefaults initializes install options with default
// control plane and proxy options. These defaults are read from the Helm
// values.yaml and values-ha.yaml files.
//
// These options may be overridden on the CLI at install-time and will be
// persisted in Linkerd's control plane configuration to be used at
// injection-time.
func newInstallOptionsWithDefaults() (*installOptions, error) {
	defaults, err := l5dcharts.NewValues(false)
	if err != nil {
		return nil, err
	}

	issuanceLifetime, err := time.ParseDuration(defaults.Identity.Issuer.IssuanceLifetime)
	if err != nil {
		return nil, err
	}

	clockSkewAllowance, err := time.ParseDuration(defaults.Identity.Issuer.ClockSkewAllowance)
	if err != nil {
		return nil, err
	}

	return &installOptions{
		clusterDomain:               defaults.Global.ClusterDomain,
		controlPlaneVersion:         version.Version,
		controllerReplicas:          defaults.ControllerReplicas,
		controllerLogLevel:          defaults.ControllerLogLevel,
		prometheusImage:             defaults.PrometheusImage,
		highAvailability:            defaults.Global.HighAvailability,
		controllerUID:               defaults.ControllerUID,
		disableH2Upgrade:            !defaults.EnableH2Upgrade,
		disableHeartbeat:            defaults.DisableHeartBeat,
		cniEnabled:                  defaults.Global.CNIEnabled,
		omitWebhookSideEffects:      defaults.OmitWebhookSideEffects,
		restrictDashboardPrivileges: defaults.RestrictDashboardPrivileges,
		controlPlaneTracing:         defaults.Global.ControlPlaneTracing,
		smiMetricsEnabled:           defaults.SMIMetrics.Enabled,
		smiMetricsImage:             defaults.SMIMetrics.Image,
		proxyConfigOptions: &proxyConfigOptions{
			proxyVersion:               version.Version,
			ignoreCluster:              false,
			proxyImage:                 defaults.Global.Proxy.Image.Name,
			initImage:                  defaults.Global.ProxyInit.Image.Name,
			initImageVersion:           version.ProxyInitVersion,
			debugImage:                 defaults.DebugContainer.Image.Name,
			debugImageVersion:          version.Version,
			dockerRegistry:             defaultDockerRegistry,
			imagePullPolicy:            defaults.Global.ImagePullPolicy,
			ignoreInboundPorts:         nil,
			ignoreOutboundPorts:        nil,
			proxyUID:                   defaults.Global.Proxy.UID,
			proxyLogLevel:              defaults.Global.Proxy.LogLevel,
			proxyControlPort:           uint(defaults.Global.Proxy.Ports.Control),
			proxyAdminPort:             uint(defaults.Global.Proxy.Ports.Admin),
			proxyInboundPort:           uint(defaults.Global.Proxy.Ports.Inbound),
			proxyOutboundPort:          uint(defaults.Global.Proxy.Ports.Outbound),
			proxyCPURequest:            defaults.Global.Proxy.Resources.CPU.Request,
			proxyMemoryRequest:         defaults.Global.Proxy.Resources.Memory.Request,
			proxyCPULimit:              defaults.Global.Proxy.Resources.CPU.Limit,
			proxyMemoryLimit:           defaults.Global.Proxy.Resources.Memory.Limit,
			enableExternalProfiles:     defaults.Global.Proxy.EnableExternalProfiles,
			waitBeforeExitSeconds:      defaults.Global.Proxy.WaitBeforeExitSeconds,
			waitHTTPFailBeforeExitPath: defaults.Global.Proxy.WaitHTTPFailBeforeExitPath,
			waitHTTPFailBeforeExitPort: defaults.Global.Proxy.WaitHTTPFailBeforeExitPort,
		},
		identityOptions: &installIdentityOptions{
			trustDomain:            defaults.Global.IdentityTrustDomain,
			issuanceLifetime:       issuanceLifetime,
			clockSkewAllowance:     clockSkewAllowance,
			identityExternalIssuer: false,
		},

		heartbeatSchedule: func() string {
			// Some of the heartbeat Prometheus queries rely on 5m resolution, which
			// means at least 5 minutes of data available. Start the first CronJob 10
			// minutes after `linkerd install` is run, to give the user 5 minutes to
			// install.
			t := time.Now().Add(10 * time.Minute).UTC()
			return fmt.Sprintf("%d %d * * * ", t.Minute(), t.Hour())
		},
	}, nil
}

// Flag configuration matrix
//
//                                 | recordableFlagSet | allStageFlagSet | installOnlyFlagSet | installPersistentFlagSet | upgradeOnlyFlagSet | "skip-checks" |
// `linkerd install`               |        X          |       X         |         X          |            X             |                    |               |
// `linkerd install config`        |                   |       X         |                    |            X             |                    |               |
// `linkerd install control-plane` |        X          |       X         |         X          |            X             |                    |       X       |
// `linkerd upgrade`               |        X          |       X         |                    |                          |          X         |               |
// `linkerd upgrade config`        |                   |       X         |                    |                          |                    |               |
// `linkerd upgrade control-plane` |        X          |       X         |                    |                          |          X         |               |
//
// allStageFlagSet is a subset of recordableFlagSet, but is also added to `linkerd [install|upgrade] config`
// proxyConfigOptions.flagSet is a subset of recordableFlagSet, and is used by `linkerd inject`.

// newCmdInstallConfig is a subcommand for `linkerd install config`
func newCmdInstallConfig(options *installOptions, parentFlags *pflag.FlagSet) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes cluster-wide resources to install Linkerd",
		Long: `Output Kubernetes cluster-wide resources to install Linkerd.

This command provides Kubernetes configs necessary to install cluster-wide
resources for the Linkerd control plane. This command should be followed by
"linkerd install control-plane".`,
		Example: `  # Default install.
  linkerd install config | kubectl apply -f -

  # Install Linkerd into a non-default namespace.
  linkerd install config -l linkerdtest | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !options.ignoreCluster {
				if err := errAfterRunningChecks(options); err != nil {
					if healthcheck.IsCategoryError(err, healthcheck.KubernetesAPIChecks) {
						fmt.Fprintf(os.Stderr, errMsgCannotInitializeClient, err)
					} else {
						fmt.Fprintf(os.Stderr, errMsgGlobalResourcesExist, err)
					}
					os.Exit(1)
				}
			}
			return installRunE(options, configStage, parentFlags)
		},
	}

	cmd.Flags().AddFlagSet(options.allStageFlagSet())

	return cmd
}

// newCmdInstallControlPlane is a subcommand for `linkerd install control-plane`
func newCmdInstallControlPlane(options *installOptions) *cobra.Command {
	// The base flags are recorded separately so that they can be serialized into
	// the configuration in validateAndBuild.
	flags := options.recordableFlagSet()
	installOnlyFlags := options.installOnlyFlagSet()

	cmd := &cobra.Command{
		Use:   "control-plane [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes control plane resources to install Linkerd",
		Long: `Output Kubernetes control plane resources to install Linkerd.

This command provides Kubernetes configs necessary to install the Linkerd
control plane. It should be run after "linkerd install config".`,
		Example: `  # Default install.
  linkerd install control-plane | kubectl apply -f -

  # Install Linkerd into a non-default namespace.
  linkerd install control-plane -l linkerdtest | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !options.skipChecks {
				// check if global resources exist to determine if the `install config`
				// stage succeeded
				if err := errAfterRunningChecks(options); err == nil {
					if healthcheck.IsCategoryError(err, healthcheck.KubernetesAPIChecks) {
						fmt.Fprintf(os.Stderr, errMsgCannotInitializeClient, err)
					} else {
						fmt.Fprintf(os.Stderr, errMsgGlobalResourcesMissing, controlPlaneNamespace)
					}
					os.Exit(1)
				}
			}

			if !options.ignoreCluster {
				if err := errIfLinkerdConfigConfigMapExists(); err != nil {
					fmt.Fprintf(os.Stderr, errMsgLinkerdConfigResourceConflict, controlPlaneNamespace, err.Error())
					os.Exit(1)
				}

			}

			return installRunE(options, controlPlaneStage, flags)
		},
	}

	cmd.PersistentFlags().BoolVar(
		&options.skipChecks, "skip-checks", options.skipChecks,
		`Skip checks for namespace existence`,
	)
	cmd.PersistentFlags().AddFlagSet(flags)
	// Some flags are not available during upgrade, etc.
	cmd.PersistentFlags().AddFlagSet(installOnlyFlags)

	return cmd
}

func newCmdInstall() *cobra.Command {
	options, err := newInstallOptionsWithDefaults()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", err)
		os.Exit(1)
	}

	// The base flags are recorded separately so that they can be serialized into
	// the configuration in validateAndBuild.
	flags := options.recordableFlagSet()
	installOnlyFlags := options.installOnlyFlagSet()
	installPersistentFlags := options.installPersistentFlagSet()

	cmd := &cobra.Command{
		Use:   "install [flags]",
		Args:  cobra.NoArgs,
		Short: "Output Kubernetes configs to install Linkerd",
		Long: `Output Kubernetes configs to install Linkerd.

This command provides all Kubernetes configs necessary to install the Linkerd
control plane.`,
		Example: `  # Default install.
  linkerd install | kubectl apply -f -

  # Install Linkerd into a non-default namespace.
  linkerd install -l linkerdtest | kubectl apply -f -

  # Installation may also be broken up into two stages by user privilege, via
  # subcommands.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !options.ignoreCluster {
				if err := errAfterRunningChecks(options); err != nil {
					if healthcheck.IsCategoryError(err, healthcheck.KubernetesAPIChecks) {
						fmt.Fprintf(os.Stderr, errMsgCannotInitializeClient, err)
					} else {
						fmt.Fprintf(os.Stderr, errMsgGlobalResourcesExist, err)
					}
					os.Exit(1)
				}
			}

			return installRunE(options, "", flags)
		},
	}

	cmd.Flags().AddFlagSet(flags)

	// Some flags are not available during upgrade, etc.
	cmd.Flags().AddFlagSet(installOnlyFlags)
	cmd.PersistentFlags().AddFlagSet(installPersistentFlags)

	cmd.AddCommand(newCmdInstallConfig(options, flags))
	cmd.AddCommand(newCmdInstallControlPlane(options))

	return cmd
}

func installRunE(options *installOptions, stage string, flags *pflag.FlagSet) error {
	values, _, err := options.validateAndBuild(stage, flags)
	if err != nil {
		return err
	}

	return render(os.Stdout, values)
}

func (options *installOptions) validateAndBuild(stage string, flags *pflag.FlagSet) (*l5dcharts.Values, *pb.All, error) {
	if err := options.validate(); err != nil {
		return nil, nil, err
	}

	options.recordFlags(flags)

	identityValues, err := options.identityOptions.validateAndBuild()
	if err != nil {
		return nil, nil, err
	}
	configs := options.configs(toIdentityContext(identityValues))

	values, err := options.buildValuesWithoutIdentity(configs)
	if err != nil {
		return nil, nil, err
	}
	values.Identity = identityValues.Identity
	values.Global.IdentityTrustAnchorsPEM = identityValues.TrustAnchorsPEM
	values.Global.IdentityTrustDomain = identityValues.TrustDomain
	values.Stage = stage

	// Update Configuration of Add-ons from config file
	err = options.UpdateAddOnValuesFromConfig(values)
	if err != nil {
		return nil, nil, err
	}

	return values, configs, nil
}

// recordableFlagSet returns flags usable during install or upgrade.
func (options *installOptions) recordableFlagSet() *pflag.FlagSet {
	e := pflag.ExitOnError

	flags := pflag.NewFlagSet("install", e)

	flags.AddFlagSet(options.proxyConfigOptions.flagSet(e))
	flags.AddFlagSet(options.allStageFlagSet())

	flags.UintVar(
		&options.controllerReplicas, "controller-replicas", options.controllerReplicas,
		"Replicas of the controller to deploy",
	)

	flags.StringVar(
		&options.controllerLogLevel, "controller-log-level", options.controllerLogLevel,
		"Log level for the controller and web components",
	)

	flags.StringVar(
		&options.prometheusImage, "prometheus-image", options.prometheusImage,
		"Custom Prometheus image name",
	)

	flags.BoolVar(
		&options.highAvailability, "ha", options.highAvailability,
		"Enable HA deployment config for the control plane (default false)",
	)
	flags.Int64Var(
		&options.controllerUID, "controller-uid", options.controllerUID,
		"Run the control plane components under this user ID",
	)
	flags.BoolVar(
		&options.disableH2Upgrade, "disable-h2-upgrade", options.disableH2Upgrade,
		"Prevents the controller from instructing proxies to perform transparent HTTP/2 upgrading (default false)",
	)
	flags.BoolVar(
		&options.disableHeartbeat, "disable-heartbeat", options.disableHeartbeat,
		"Disables the heartbeat cronjob (default false)",
	)
	flags.DurationVar(
		&options.identityOptions.issuanceLifetime, "identity-issuance-lifetime", options.identityOptions.issuanceLifetime,
		"The amount of time for which the Identity issuer should certify identity",
	)
	flags.DurationVar(
		&options.identityOptions.clockSkewAllowance, "identity-clock-skew-allowance", options.identityOptions.clockSkewAllowance,
		"The amount of time to allow for clock skew within a Linkerd cluster",
	)
	flags.BoolVar(
		&options.omitWebhookSideEffects, "omit-webhook-side-effects", options.omitWebhookSideEffects,
		"Omit the sideEffects flag in the webhook manifests, This flag must be provided during install or upgrade for Kubernetes versions pre 1.12",
	)
	flags.BoolVar(
		&options.controlPlaneTracing, "control-plane-tracing", options.controlPlaneTracing,
		"Enables Control Plane Tracing with the defaults",
	)
	flags.StringVar(
		&options.identityOptions.crtPEMFile, "identity-issuer-certificate-file", options.identityOptions.crtPEMFile,
		"A path to a PEM-encoded file containing the Linkerd Identity issuer certificate (generated by default)",
	)
	flags.StringVar(
		&options.identityOptions.keyPEMFile, "identity-issuer-key-file", options.identityOptions.keyPEMFile,
		"A path to a PEM-encoded file containing the Linkerd Identity issuer private key (generated by default)",
	)
	flags.StringVar(
		&options.identityOptions.trustPEMFile, "identity-trust-anchors-file", options.identityOptions.trustPEMFile,
		"A path to a PEM-encoded file containing Linkerd Identity trust anchors (generated by default)",
	)

	flags.BoolVar(
		&options.smiMetricsEnabled, "smi-metrics", options.smiMetricsEnabled,
		"Enables installing the SMI-Metrics controller",
	)

	flags.StringVarP(&options.controlPlaneVersion, "control-plane-version", "", options.controlPlaneVersion, "Tag to be used for the control plane component images")
	flags.StringVar(&options.smiMetricsImage, "smi-metrics-image", options.smiMetricsImage, "SMI Metrics image")

	// Hide developer focused flags in release builds.
	release, err := version.IsReleaseChannel(version.Version)
	if err != nil {
		log.Errorf("Unable to parse version: %s", version.Version)
	}
	if release {
		flags.MarkHidden("control-plane-version")
		flags.MarkHidden("proxy-image")
		flags.MarkHidden("proxy-version")
		flags.MarkHidden("image-pull-policy")
		flags.MarkHidden("init-image")
		flags.MarkHidden("init-image-version")
	}

	flags.MarkHidden("control-plane-tracing")
	flags.MarkHidden("smi-metrics")
	flags.MarkHidden("smi-metrics-image")

	return flags
}

// allStageFlagSet returns flags usable for single and multi-stage  installs and
// upgrades. For multi-stage installs, users must set these flags consistently
// across commands.
func (options *installOptions) allStageFlagSet() *pflag.FlagSet {
	flags := pflag.NewFlagSet("all-stage", pflag.ExitOnError)

	flags.BoolVar(&options.cniEnabled, "linkerd-cni-enabled", options.cniEnabled,
		"Omit the NET_ADMIN capability in the PSP and the proxy-init container when injecting the proxy; requires the linkerd-cni plugin to already be installed",
	)

	flags.BoolVar(
		&options.restrictDashboardPrivileges, "restrict-dashboard-privileges", options.restrictDashboardPrivileges,
		"Restrict the Linkerd Dashboard's default privileges to disallow Tap and Check",
	)

	flags.StringVar(
		&options.addOnConfig, "addon-config", options.addOnConfig,
		"A path to a configuration file of add-ons. If add-on config already exists, this new config gets merged with the existing one (unless --addon-overwrite is used)",
	)

	return flags
}

// installOnlyFlagSet includes flags that are only accessible at install-time
// and not at upgrade-time.
func (options *installOptions) installOnlyFlagSet() *pflag.FlagSet {
	flags := pflag.NewFlagSet("install-only", pflag.ExitOnError)

	flags.StringVar(
		&options.clusterDomain, "cluster-domain", options.clusterDomain,
		"Set custom cluster domain",
	)
	flags.StringVar(
		&options.identityOptions.trustDomain, "identity-trust-domain", options.identityOptions.trustDomain,
		"Configures the name suffix used for identities.",
	)
	flags.BoolVar(
		&options.identityOptions.identityExternalIssuer, "identity-external-issuer", options.identityOptions.identityExternalIssuer,
		"Whether to use an external identity issuer (default false)",
	)
	return flags
}

// installPersistentFlagSet includes flags that are only accessible at
// install-time, not at upgrade-time, and are also used by install subcommands.
func (options *installOptions) installPersistentFlagSet() *pflag.FlagSet {
	flags := pflag.NewFlagSet("install-persist", pflag.ExitOnError)

	flags.BoolVar(
		&options.ignoreCluster, "ignore-cluster", options.ignoreCluster,
		"Ignore the current Kubernetes cluster when checking for existing cluster configuration (default false)",
	)

	return flags
}

// UpdateAddOnValuesFromConfig takes a values struct and updates its add-on values from the config installOption
func (options *installOptions) UpdateAddOnValuesFromConfig(values *l5dcharts.Values) error {

	if options.addOnConfig != "" {
		addOnValues, err := ioutil.ReadFile(options.addOnConfig)
		if err != nil {
			return err
		}

		rawValues, err := yaml.Marshal(values)
		if err != nil {
			return err
		}
		// Merge Add-On Values with Values
		finalValues, err := mergeRaw(rawValues, addOnValues)
		if err != nil {
			return err
		}

		if err = yaml.Unmarshal(finalValues, values); err != nil {
			return nil
		}

	}

	return nil
}

func mergeRaw(a, b []byte) ([]byte, error) {
	var aMap, bMap chartutil.Values

	err := yaml.Unmarshal(a, &aMap)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(b, &bMap)
	if err != nil {
		return nil, err
	}

	aMap.MergeInto(bMap)
	return yaml.Marshal(aMap)

}

func (options *installOptions) recordFlags(flags *pflag.FlagSet) {
	if flags == nil {
		return
	}

	flags.VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			switch f.Name {
			case "ignore-cluster", "control-plane-version", "proxy-version", "identity-issuer-certificate-file", "identity-issuer-key-file", "identity-trust-anchors-file", "addon-config":
				// These flags don't make sense to record.
			default:
				options.recordedFlags = append(options.recordedFlags, &pb.Install_Flag{
					Name:  f.Name,
					Value: f.Value.String(),
				})
			}
		}
	})
}

func (options *installOptions) validate() error {
	if options.ignoreCluster && options.identityOptions.identityExternalIssuer {
		return errors.New("--ignore-cluster is not supported when --identity-external-issuer=true")
	}

	if options.controlPlaneVersion != "" && !alphaNumDashDot.MatchString(options.controlPlaneVersion) {
		return fmt.Errorf("%s is not a valid version", options.controlPlaneVersion)
	}

	if options.identityOptions == nil {
		// Programmer error: identityOptions may be empty, but it must be set by the constructor.
		panic("missing identity options")
	}

	if _, err := log.ParseLevel(options.controllerLogLevel); err != nil {
		return fmt.Errorf("--controller-log-level must be one of: panic, fatal, error, warn, info, debug")
	}

	if options.prometheusImage != "" && !alphaNumDashDotSlashColonUnderscore.MatchString(options.prometheusImage) {
		return fmt.Errorf("%s is not a valid prometheus image", options.prometheusImage)
	}

	if err := options.proxyConfigOptions.validate(); err != nil {
		return err
	}
	if options.proxyLogLevel == "" {
		return errors.New("--proxy-log-level must not be empty")
	}

	return nil
}

// buildValuesWithoutIdentity builds the values that will be used to render
// the Helm templates. It overrides the defaults values with CLI options.
func (options *installOptions) buildValuesWithoutIdentity(configs *pb.All) (*l5dcharts.Values, error) {
	installValues, err := l5dcharts.NewValues(options.highAvailability)
	if err != nil {
		return nil, err
	}

	if options.highAvailability {
		// use the HA defaults if CLI options aren't provided
		if options.controllerReplicas == 1 {
			options.controllerReplicas = installValues.ControllerReplicas
		}

		if options.proxyCPURequest == "" {
			options.proxyCPURequest = installValues.Global.Proxy.Resources.CPU.Request
		}

		if options.proxyMemoryRequest == "" {
			options.proxyMemoryRequest = installValues.Global.Proxy.Resources.Memory.Request
		}

		if options.proxyCPULimit == "" {
			options.proxyCPULimit = installValues.Global.Proxy.Resources.CPU.Limit
		}

		if options.proxyMemoryLimit == "" {
			options.proxyMemoryLimit = installValues.Global.Proxy.Resources.Memory.Limit
		}

		// `configs` was built before the HA option is evaluated, so we need
		// to make sure the HA proxy resources are added here.
		if configs.Proxy.Resource.RequestCpu == "" {
			configs.Proxy.Resource.RequestCpu = options.proxyCPURequest
		}

		if configs.Proxy.Resource.RequestMemory == "" {
			configs.Proxy.Resource.RequestMemory = options.proxyMemoryRequest
		}

		if configs.Proxy.Resource.LimitCpu == "" {
			configs.Proxy.Resource.LimitCpu = options.proxyCPULimit
		}

		if configs.Proxy.Resource.LimitMemory == "" {
			configs.Proxy.Resource.LimitMemory = options.proxyMemoryLimit
		}

		options.identityOptions.replicas = options.controllerReplicas
	}

	globalJSON, proxyJSON, installJSON, err := config.ToJSON(configs)
	if err != nil {
		return nil, err
	}

	// override default values with CLI options
	installValues.Global.ClusterDomain = configs.GetGlobal().GetClusterDomain()
	installValues.Configs.Global = globalJSON
	installValues.Configs.Proxy = proxyJSON
	installValues.Configs.Install = installJSON
	installValues.ControllerImage = fmt.Sprintf("%s/controller", options.dockerRegistry)
	installValues.Global.ControllerImageVersion = configs.GetGlobal().GetVersion()
	installValues.ControllerLogLevel = options.controllerLogLevel
	installValues.ControllerReplicas = options.controllerReplicas
	installValues.ControllerUID = options.controllerUID
	installValues.Global.ControlPlaneTracing = options.controlPlaneTracing
	installValues.EnableH2Upgrade = !options.disableH2Upgrade
	installValues.EnablePodAntiAffinity = options.highAvailability
	installValues.Global.HighAvailability = options.highAvailability
	installValues.Global.ImagePullPolicy = options.imagePullPolicy
	installValues.Grafana["image"].(map[string]interface{})["name"] = fmt.Sprintf("%s/grafana", options.dockerRegistry)
	installValues.Grafana["image"].(map[string]interface{})["version"] = options.controlPlaneVersion
	if options.prometheusImage != "" {
		installValues.PrometheusImage = options.prometheusImage
	}
	installValues.Global.Namespace = controlPlaneNamespace
	installValues.Global.CNIEnabled = options.cniEnabled
	installValues.OmitWebhookSideEffects = options.omitWebhookSideEffects
	installValues.PrometheusLogLevel = toPromLogLevel(strings.ToLower(options.controllerLogLevel))
	installValues.HeartbeatSchedule = options.heartbeatSchedule()
	installValues.RestrictDashboardPrivileges = options.restrictDashboardPrivileges
	installValues.DisableHeartBeat = options.disableHeartbeat
	installValues.WebImage = fmt.Sprintf("%s/web", options.dockerRegistry)
	installValues.SMIMetrics.Image = options.smiMetricsImage
	installValues.SMIMetrics.Enabled = options.smiMetricsEnabled

	installValues.Global.Proxy = &l5dcharts.Proxy{
		EnableExternalProfiles: options.enableExternalProfiles,
		Image: &l5dcharts.Image{
			Name:       registryOverride(options.proxyImage, options.dockerRegistry),
			PullPolicy: options.imagePullPolicy,
			Version:    options.proxyVersion,
		},
		LogLevel: options.proxyLogLevel,
		Ports: &l5dcharts.Ports{
			Admin:    int32(options.proxyAdminPort),
			Control:  int32(options.proxyControlPort),
			Inbound:  int32(options.proxyInboundPort),
			Outbound: int32(options.proxyOutboundPort),
		},
		Resources: &l5dcharts.Resources{
			CPU: l5dcharts.Constraints{
				Limit:   options.proxyCPULimit,
				Request: options.proxyCPURequest,
			},
			Memory: l5dcharts.Constraints{
				Limit:   options.proxyMemoryLimit,
				Request: options.proxyMemoryRequest,
			},
		},
		UID:   options.proxyUID,
		Trace: installValues.Global.Proxy.Trace,
	}

	installValues.Global.ProxyInit.Image.Name = registryOverride(options.initImage, options.dockerRegistry)
	installValues.Global.ProxyInit.Image.PullPolicy = options.imagePullPolicy
	installValues.Global.ProxyInit.Image.Version = options.initImageVersion
	installValues.Global.ProxyInit.IgnoreInboundPorts = strings.Join(options.ignoreInboundPorts, ",")
	installValues.Global.ProxyInit.IgnoreOutboundPorts = strings.Join(options.ignoreOutboundPorts, ",")

	installValues.DebugContainer.Image.Name = registryOverride(options.debugImage, options.dockerRegistry)
	installValues.DebugContainer.Image.PullPolicy = options.imagePullPolicy
	installValues.DebugContainer.Image.Version = options.debugImageVersion

	return installValues, nil
}

func toPromLogLevel(level string) string {
	switch level {
	case "panic", "fatal":
		return "error"
	default:
		return level
	}
}

func render(w io.Writer, values *l5dcharts.Values) error {
	// Render raw values and create chart config
	rawValues, err := yaml.Marshal(values)
	if err != nil {
		return err
	}

	files := []*chartutil.BufferedFile{
		{Name: chartutil.ChartfileName},
	}

	addOns, err := l5dcharts.ParseAddOnValues(values)
	if err != nil {
		return err
	}

	// Initialize add-on sub-charts
	addOnCharts := make(map[string]*charts.Chart)
	for _, addOn := range addOns {
		addOnCharts[addOn.Name()] = &charts.Chart{
			Name:      addOn.Name(),
			Dir:       addOnChartsPath + "/" + addOn.Name(),
			Namespace: controlPlaneNamespace,
			RawValues: append(addOn.Values(), rawValues...),
			Files: []*chartutil.BufferedFile{&chartutil.BufferedFile{
				Name: chartutil.ChartfileName,
			}},
		}
	}

	if values.Stage == "" || values.Stage == configStage {
		for _, template := range templatesConfigStage {
			files = append(files,
				&chartutil.BufferedFile{Name: template},
			)
		}

		// Fill add-on's sub-charts with config templates
		for _, addOn := range addOns {
			addOnCharts[addOn.Name()].Files = append(addOnCharts[addOn.Name()].Files, addOn.ConfigStageTemplates()...)
		}
	}

	if values.Stage == "" || values.Stage == controlPlaneStage {
		files = append(files, &chartutil.BufferedFile{Name: "smi-metrics-config.yaml"})
		for _, template := range templatesControlPlaneStage {
			files = append(files,
				&chartutil.BufferedFile{Name: template},
			)
		}

		// Fill add-on's sub-charts with control-plane templates
		for _, addOn := range addOns {
			addOnCharts[addOn.Name()].Files = append(addOnCharts[addOn.Name()].Files, addOn.ControlPlaneStageTemplates()...)
		}

	}

	// TODO refactor to use l5dcharts.LoadChart()
	chart := &charts.Chart{
		Name:      helmDefaultChartName,
		Dir:       helmDefaultChartDir,
		Namespace: controlPlaneNamespace,
		RawValues: rawValues,
		Files:     files,
	}
	buf, err := chart.Render()
	if err != nil {
		return err
	}

	for _, addon := range addOns {
		b, err := addOnCharts[addon.Name()].Render()
		if err != nil {
			return err
		}

		if _, err := buf.WriteString(b.String()); err != nil {
			return err
		}
	}

	_, err = w.Write(buf.Bytes())
	return err
}

func (options *installOptions) configs(identity *pb.IdentityContext) *pb.All {
	return &pb.All{
		Global:  options.globalConfig(identity),
		Proxy:   options.proxyConfig(),
		Install: options.installConfig(),
	}
}

func (options *installOptions) globalConfig(identity *pb.IdentityContext) *pb.Global {
	return &pb.Global{
		LinkerdNamespace:       controlPlaneNamespace,
		CniEnabled:             options.cniEnabled,
		Version:                options.controlPlaneVersion,
		IdentityContext:        identity,
		OmitWebhookSideEffects: options.omitWebhookSideEffects,
		ClusterDomain:          options.clusterDomain,
	}
}

func (options *installOptions) installConfig() *pb.Install {
	return &pb.Install{
		CliVersion: version.Version,
		Flags:      options.recordedFlags,
	}
}

func (options *installOptions) proxyConfig() *pb.Proxy {
	ignoreInboundPorts := []*pb.PortRange{}
	for _, portOrRange := range options.ignoreInboundPorts {
		ignoreInboundPorts = append(ignoreInboundPorts, &pb.PortRange{PortRange: portOrRange})
	}

	ignoreOutboundPorts := []*pb.PortRange{}
	for _, portOrRange := range options.ignoreOutboundPorts {
		ignoreOutboundPorts = append(ignoreOutboundPorts, &pb.PortRange{PortRange: portOrRange})
	}

	return &pb.Proxy{
		ProxyImage: &pb.Image{
			ImageName:  registryOverride(options.proxyImage, options.dockerRegistry),
			PullPolicy: options.imagePullPolicy,
		},
		ProxyInitImage: &pb.Image{
			ImageName:  registryOverride(options.initImage, options.dockerRegistry),
			PullPolicy: options.imagePullPolicy,
		},
		ControlPort: &pb.Port{
			Port: uint32(options.proxyControlPort),
		},
		IgnoreInboundPorts:  ignoreInboundPorts,
		IgnoreOutboundPorts: ignoreOutboundPorts,
		InboundPort: &pb.Port{
			Port: uint32(options.proxyInboundPort),
		},
		AdminPort: &pb.Port{
			Port: uint32(options.proxyAdminPort),
		},
		OutboundPort: &pb.Port{
			Port: uint32(options.proxyOutboundPort),
		},
		Resource: &pb.ResourceRequirements{
			RequestCpu:    options.proxyCPURequest,
			RequestMemory: options.proxyMemoryRequest,
			LimitCpu:      options.proxyCPULimit,
			LimitMemory:   options.proxyMemoryLimit,
		},
		ProxyUid: options.proxyUID,
		LogLevel: &pb.LogLevel{
			Level: options.proxyLogLevel,
		},
		DisableExternalProfiles: !options.enableExternalProfiles,
		ProxyVersion:            options.proxyVersion,
		ProxyInitImageVersion:   options.initImageVersion,
		DebugImage: &pb.Image{
			ImageName:  registryOverride(options.debugImage, options.dockerRegistry),
			PullPolicy: options.imagePullPolicy,
		},
		DebugImageVersion: options.debugImageVersion,
	}
}

func errAfterRunningChecks(options *installOptions) error {
	checks := []healthcheck.CategoryID{
		healthcheck.KubernetesAPIChecks,
		healthcheck.LinkerdPreInstallGlobalResourcesChecks,
	}
	hc := healthcheck.NewHealthChecker(checks, &healthcheck.Options{
		ControlPlaneNamespace: controlPlaneNamespace,
		KubeConfig:            kubeconfigPath,
		Impersonate:           impersonate,
		ImpersonateGroup:      impersonateGroup,
		KubeContext:           kubeContext,
		APIAddr:               apiAddr,
		CNIEnabled:            options.cniEnabled,
	})

	var k8sAPIError error
	errMsgs := []string{}
	hc.RunChecks(func(result *healthcheck.CheckResult) {
		if result.Err != nil {
			if ce, ok := result.Err.(*healthcheck.CategoryError); ok {
				if ce.Category == healthcheck.KubernetesAPIChecks {
					k8sAPIError = ce
				} else if re, ok := ce.Err.(*healthcheck.ResourceError); ok {
					// resource error, print in kind.group/name format
					for _, res := range re.Resources {
						errMsgs = append(errMsgs, res.String())
					}
				} else {
					// unknown category error, just print it
					errMsgs = append(errMsgs, result.Err.Error())
				}
			} else {
				// unknown error, just print it
				errMsgs = append(errMsgs, result.Err.Error())
			}
		}
	})

	// errors from the KubernetesAPIChecks category take precedence
	if k8sAPIError != nil {
		return k8sAPIError
	}

	if len(errMsgs) > 0 {
		return errors.New(strings.Join(errMsgs, "\n"))
	}

	return nil
}

func errIfLinkerdConfigConfigMapExists() error {
	kubeAPI, err := k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, impersonateGroup, 0)
	if err != nil {
		return err
	}

	_, err = kubeAPI.CoreV1().Namespaces().Get(controlPlaneNamespace, metav1.GetOptions{})
	if err != nil {
		return err
	}

	_, _, err = healthcheck.FetchLinkerdConfigMap(kubeAPI, controlPlaneNamespace)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	return fmt.Errorf("'linkerd-config' config map already exists")
}

func checkFilesExist(files []string) error {
	for _, f := range files {
		stat, err := os.Stat(f)
		if err != nil {
			return fmt.Errorf("missing file: %s", err)
		}
		if stat.IsDir() {
			return fmt.Errorf("not a file: %s", f)
		}
	}
	return nil
}

func (idopts *installIdentityOptions) validate() error {
	if idopts == nil {
		return nil
	}

	if idopts.trustDomain != "" {
		if errs := validation.IsDNS1123Subdomain(idopts.trustDomain); len(errs) > 0 {
			return fmt.Errorf("invalid trust domain '%s': %s", idopts.trustDomain, errs[0])
		}
	}

	if idopts.identityExternalIssuer {

		if idopts.crtPEMFile != "" {
			return errors.New("--identity-issuer-certificate-file must not be specified if --identity-external-issuer=true")
		}

		if idopts.keyPEMFile != "" {
			return errors.New("--identity-issuer-key-file must not be specified if --identity-external-issuer=true")
		}

		if idopts.trustPEMFile != "" {
			return errors.New("--identity-trust-anchors-file must not be specified if --identity-external-issuer=true")
		}

	} else {
		if idopts.trustPEMFile != "" || idopts.crtPEMFile != "" || idopts.keyPEMFile != "" {
			if idopts.trustPEMFile == "" {
				return errors.New("a trust anchors file must be specified if other credentials are provided")
			}
			if idopts.crtPEMFile == "" {
				return errors.New("a certificate file must be specified if other credentials are provided")
			}
			if idopts.keyPEMFile == "" {
				return errors.New("a private key file must be specified if other credentials are provided")
			}
			if err := checkFilesExist([]string{idopts.trustPEMFile, idopts.crtPEMFile, idopts.keyPEMFile}); err != nil {
				return err
			}
		}
	}

	return nil
}

func (idopts *installIdentityOptions) validateAndBuild() (*identityWithAnchorsAndTrustDomain, error) {
	if idopts == nil {
		return nil, nil
	}

	if err := idopts.validate(); err != nil {
		return nil, err
	}

	if idopts.identityExternalIssuer {
		return idopts.readExternallyManaged()
	} else if idopts.trustPEMFile != "" && idopts.crtPEMFile != "" && idopts.keyPEMFile != "" {
		return idopts.readValues()
	} else {
		return idopts.genValues()
	}
}

func (idopts *installIdentityOptions) issuerName() string {
	return fmt.Sprintf("identity.%s.%s", controlPlaneNamespace, idopts.trustDomain)
}

func (idopts *installIdentityOptions) genValues() (*identityWithAnchorsAndTrustDomain, error) {
	root, err := tls.GenerateRootCAWithDefaults(idopts.issuerName())
	if err != nil {
		return nil, fmt.Errorf("failed to generate root certificate for identity: %s", err)
	}

	return &identityWithAnchorsAndTrustDomain{
		TrustDomain:     idopts.trustDomain,
		TrustAnchorsPEM: root.Cred.Crt.EncodeCertificatePEM(),
		Identity: &l5dcharts.Identity{
			Issuer: &l5dcharts.Issuer{
				Scheme:              consts.IdentityIssuerSchemeLinkerd,
				ClockSkewAllowance:  idopts.clockSkewAllowance.String(),
				IssuanceLifetime:    idopts.issuanceLifetime.String(),
				CrtExpiry:           root.Cred.Crt.Certificate.NotAfter,
				CrtExpiryAnnotation: k8s.IdentityIssuerExpiryAnnotation,
				TLS: &l5dcharts.TLS{
					KeyPEM: root.Cred.EncodePrivateKeyPEM(),
					CrtPEM: root.Cred.Crt.EncodeCertificatePEM(),
				},
			},
		},
	}, nil
}

func (idopts *installIdentityOptions) readExternallyManaged() (*identityWithAnchorsAndTrustDomain, error) {

	kubeAPI, err := k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, impersonateGroup, 0)
	if err != nil {
		return nil, fmt.Errorf("error fetching external issuer config: %s", err)
	}

	externalIssuerData, err := issuercerts.FetchExternalIssuerData(kubeAPI, controlPlaneNamespace)
	if err != nil {
		return nil, err
	}
	_, err = externalIssuerData.VerifyAndBuildCreds(idopts.issuerName())
	if err != nil {
		return nil, fmt.Errorf("failed to read CA from %s: %s", consts.IdentityIssuerSecretName, err)
	}

	return &identityWithAnchorsAndTrustDomain{
		TrustDomain:     idopts.trustDomain,
		TrustAnchorsPEM: externalIssuerData.TrustAnchors,
		Identity: &l5dcharts.Identity{
			Issuer: &l5dcharts.Issuer{
				Scheme:             string(corev1.SecretTypeTLS),
				ClockSkewAllowance: idopts.clockSkewAllowance.String(),
				IssuanceLifetime:   idopts.issuanceLifetime.String(),
			},
		},
	}, nil

}

// readValues attempts to read an issuer configuration from disk
// to produce an `installIdentityValues`.
//
// The identity options must have already been validated.
func (idopts *installIdentityOptions) readValues() (*identityWithAnchorsAndTrustDomain, error) {
	issuerData, err := issuercerts.LoadIssuerDataFromFiles(idopts.keyPEMFile, idopts.crtPEMFile, idopts.trustPEMFile)
	if err != nil {
		return nil, err
	}

	creds, err := issuerData.VerifyAndBuildCreds(idopts.issuerName())
	if err != nil {
		return nil, fmt.Errorf("failed to verify issuer certs stored on disk: %s", err)
	}

	return &identityWithAnchorsAndTrustDomain{
		TrustDomain:     idopts.trustDomain,
		TrustAnchorsPEM: issuerData.TrustAnchors,
		Identity: &l5dcharts.Identity{
			Issuer: &l5dcharts.Issuer{
				Scheme:              consts.IdentityIssuerSchemeLinkerd,
				ClockSkewAllowance:  idopts.clockSkewAllowance.String(),
				IssuanceLifetime:    idopts.issuanceLifetime.String(),
				CrtExpiry:           creds.Crt.Certificate.NotAfter,
				CrtExpiryAnnotation: k8s.IdentityIssuerExpiryAnnotation,
				TLS: &l5dcharts.TLS{
					KeyPEM: creds.EncodePrivateKeyPEM(),
					CrtPEM: creds.EncodeCertificatePEM(),
				},
			},
		},
	}, nil
}

func toIdentityContext(idvals *identityWithAnchorsAndTrustDomain) *pb.IdentityContext {
	if idvals == nil {
		return nil
	}

	il, err := time.ParseDuration(idvals.Identity.Issuer.IssuanceLifetime)
	if err != nil {
		il = defaultIdentityIssuanceLifetime
	}

	csa, err := time.ParseDuration(idvals.Identity.Issuer.ClockSkewAllowance)
	if err != nil {
		csa = defaultIdentityClockSkewAllowance
	}

	return &pb.IdentityContext{
		TrustDomain:        idvals.TrustDomain,
		TrustAnchorsPem:    idvals.TrustAnchorsPEM,
		IssuanceLifetime:   ptypes.DurationProto(il),
		ClockSkewAllowance: ptypes.DurationProto(csa),
		Scheme:             idvals.Identity.Issuer.Scheme,
	}
}
