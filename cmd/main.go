/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	goflag "flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/scalefromzero"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/steadystate"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpuusage"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	prometheusutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scaler"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/crd"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool"
	warmpoolpolicy "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/policy"
	warmpoolpool "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/warmpool/pool"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	corev1 "k8s.io/api/core/v1"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	inferencePoolV1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	inferencePoolV1alpha2 "sigs.k8s.io/gateway-api-inference-extension/apix/v1alpha2"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(promoperator.AddToScheme(scheme))
	utilruntime.Must(inferencePoolV1.Install(scheme))
	utilruntime.Must(inferencePoolV1alpha2.Install(scheme))
	// KEDA scheme is registered unconditionally so the client can list ScaledObjects
	// when the CRD is present. Listing fails gracefully (NoMatchError) when not installed.
	utilruntime.Must(kedav1alpha1.AddToScheme(scheme))
	// Note: LeaderWorkerSet scheme is added conditionally in main() after checking if CRD exists
	// +kubebuilder:scaffold:scheme
}

// defaultLogVerbosity is the -v the shipped deployment runs at: config/base/manager
// passes no -v, so this value decides which V() levels reach the logs. The
// saturation analyzer's per-replica capacity lines sit at V(logging.DEFAULT) and
// hack/benchmark/dump_k2_decisions.py needs them, so lowering this silently empties
// that report. TestDefaultVerbosityKeepsCapacityLogsVisible guards the relationship.
const defaultLogVerbosity = logging.DEFAULT

// nolint:gocyclo
func main() {
	// Command-line flags

	loggerVerbosity := flag.Int("v", defaultLogVerbosity, "number for the log level verbosity")

	configFilePath := flag.String("config-file", "", "Path to the YAML configuration file. "+
		"When set, the main configuration is read from this file instead of a Kubernetes ConfigMap.")

	flag.String("metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.Bool("leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.Bool("metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.String("webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.String("webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.String("webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.String("metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.String("metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.String("metrics-cert-key", "tls.key", "The name of the metrics key file.")
	flag.String("watch-namespace", "",
		"Namespace to watch for updates. If unspecified, all namespaces are watched.")

	externalScalerBindAddress := flag.String("external-scaler-bind-address", ":9090",
		"The address the KEDA external scaler gRPC service binds to. "+
			"KEDA ScaledObjects reference this via an external trigger's scalerAddress.")

	// The warm pool is OFF unless a namespace is named, because it holds GPUs
	// continuously: that is a cost decision an operator makes, never a default.
	warmPoolNamespace := flag.String("warm-pool-namespace", "",
		"Namespace holding warm-pool Pods. Empty disables the pool entirely.")
	warmPoolSleepMinSize := flag.Int("warm-pool-sleep-min-size", 1,
		"Floor on FREE pool Pods -- ones with every instance asleep. This is the reserve "+
			"the pool keeps for the next spike, per pool rather than per model.")
	warmPoolMaxHold := flag.Duration("warm-pool-max-hold", 2*time.Minute,
		"How long a borrowed Pod may serve before it is returned regardless. Bounds the case "+
			"where the ordinary replicas never arrive, which would otherwise turn insurance "+
			"into permanent capacity for one variant.")

	// Leader election timeout configuration flags
	// These can be overridden in manager.yaml to tune for different environments
	// (e.g., higher values for environments with network latency or API server slowness)
	flag.Duration("leader-election-lease-duration", 60*time.Second,
		"The duration that non-leader candidates will wait to force acquire leadership. "+
			"Increased from default 15s to 60s to prevent lease renewal failures in environments with network latency.")
	flag.Duration("leader-election-renew-deadline", 50*time.Second,
		"The duration that the acting master will retry refreshing leadership before giving up. "+
			"Increased from default 10s to 50s to provide more tolerance for network latency and API server delays.")
	flag.Duration("leader-election-retry-period", 10*time.Second,
		"The duration the clients should wait between tries of actions. "+
			"Increased from default 2s to 10s to reduce API server load and provide more time between renewal attempts.")
	flag.Duration("rest-client-timeout", 60*time.Second,
		"The timeout for REST API calls to the Kubernetes API server. "+
			"Increased from default ~30s to 60s for better resilience against network latency.")

	opts := ctrlzap.Options{
		Development: true,
	}
	gfs := goflag.NewFlagSet("zap", goflag.ExitOnError)
	opts.BindFlags(gfs) // zap expects a standard Go FlagSet and pflag.FlagSet is not compatible.
	flag.CommandLine.AddGoFlagSet(gfs)

	flag.Parse()

	logging.InitLogging(&opts, loggerVerbosity)
	defer logging.Sync() // nolint:errcheck

	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("Logger initialized")

	// Get REST config early (needed for config loading)
	restConfig := ctrl.GetConfigOrDie()

	// Load unified configuration (fail-fast if invalid)
	// Viper resolves precedence: flags > env > config file > defaults
	// For more information see:
	// https://github.com/llm-d/llm-d-workload-variant-autoscaler/blob/main/docs/user-guide/configuration.md
	cfg, err := config.Load(flag.CommandLine, *configFilePath)
	if err != nil {
		setupLog.Error(err, "failed to load configuration - this is a fatal error")
		logging.Sync() //nolint:errcheck
		os.Exit(1)     //nolint:gocritic // exitAfterDefer: Sync() called explicitly above
	}
	setupLog.Info("Configuration loaded successfully")

	// Where cluster policy comes from has to be settled BEFORE the manager exists,
	// because the manager's cache is built from the answer — a namespace absent
	// from the cache is a namespace whose limiters can never be read. It needs a
	// live lookup (the well-known namespace only wins if it is actually there), so
	// it uses a direct client rather than the manager's, which is not running yet.
	policyProbe, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to build a client to resolve the cluster policy namespace")
		os.Exit(1)
	}
	// Not ctrl.SetupSignalHandler(): that may be called only once per process, and
	// mgr.Start() is its rightful owner. A second call panics.
	if err := controller.ResolvePolicyNamespace(context.Background(), policyProbe, cfg); err != nil {
		setupLog.Error(err, "unable to resolve where cluster policy comes from")
		os.Exit(1)
	}

	// Conditionally add LeaderWorkerSet scheme if CRD exists
	lwsEnabled := crd.CheckLeaderWorkerSetCRD(restConfig, setupLog)
	if lwsEnabled {
		if err := lwsv1.AddToScheme(scheme); err != nil {
			setupLog.Error(err, "failed to add LeaderWorkerSet scheme")
			os.Exit(1)
		}
		setupLog.Info("LeaderWorkerSet CRD detected - support enabled")
	} else {
		setupLog.Info("LeaderWorkerSet CRD not found - support disabled (Deployment-only mode)")
	}

	// KEDA is not optional: WVA discovers the workloads it manages by being called
	// about them over the external-scaler contract, and KEDA is what makes those
	// calls. Without it nothing ever registers, so WVA runs and manages nothing —
	// which is quiet enough to be worth saying loudly here.
	//
	// The CRD check is a diagnostic only. Nothing branches on it: there is no
	// ScaledObject watch, index or cached read left to gate (see
	// docs/plans/engine/keda-driven-discovery.md).
	if crd.CheckKEDACRD(restConfig, setupLog) {
		setupLog.Info("KEDA ScaledObject CRD detected - WVA will be discovered by KEDA calling its external scaler")
	} else {
		setupLog.Info("WARNING: KEDA ScaledObject CRD not found. WVA discovers workloads only when KEDA " +
			"calls its external scaler, so nothing will be discovered or scaled until KEDA is installed " +
			"and a ScaledObject names this scaler in a trigger.")
	}

	tlsOpts := []func(*tls.Config){
		func(c *tls.Config) {
			c.NextProtos = []string{"h2", "http/1.1"}
		},
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(cfg.WebhookCertPath()) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhookCertPath", cfg.WebhookCertPath(),
			"webhookCertName", cfg.WebhookCertName(),
			"webhookCertKey", cfg.WebhookCertKey())

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.WebhookCertPath(), cfg.WebhookCertName()),
			filepath.Join(cfg.WebhookCertPath(), cfg.WebhookCertKey()),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/base/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.MetricsAddr(),
		SecureServing: cfg.SecureMetrics(),
		TLSOpts:       tlsOpts,
	}

	if cfg.SecureMetrics() {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/base/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/base/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/base/monitoring/kustomization.yaml for TLS certification.
	if len(cfg.MetricsCertPath()) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metricsCertPath", cfg.MetricsCertPath(),
			"metricsCertName", cfg.MetricsCertName(),
			"metricsCertKey", cfg.MetricsCertKey(),
		)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.MetricsCertPath(), cfg.MetricsCertName()),
			filepath.Join(cfg.MetricsCertPath(), cfg.MetricsCertKey()),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize metrics certificate watcher")
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// --- Setup Datastore ---
	ds := datastore.NewDatastore(cfg)

	// Use configurable REST client timeout from Config (default 60s, can be overridden via --rest-client-timeout flag)
	restConfig.Timeout = cfg.RestTimeout()

	// Configure leader election with configurable timeouts to prevent lease renewal failures
	// Default values are: LeaseDuration=60s, RenewDeadline=50s, RetryPeriod=10s
	// These can be overridden via command-line flags in manager.yaml
	// Increased from controller-runtime defaults (15s, 10s, 2s) to provide more tolerance
	// for network latency and API server delays

	leaseDurationVal := cfg.LeaseDuration()
	renewDeadlineVal := cfg.RenewDeadline()
	retryPeriodVal := cfg.RetryPeriod()
	mgrOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.ProbeAddr(),
		LeaderElection:         cfg.EnableLeaderElection(),
		LeaderElectionID:       cfg.LeaderElectionID(),
		// Leader election timeout configuration (from Config, can be overridden via flags/env/ConfigMap)
		LeaseDuration: &leaseDurationVal,
		RenewDeadline: &renewDeadlineVal,
		RetryPeriod:   &retryPeriodVal,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// This is safe to enable because the program ends immediately after the manager stops
		// (see mgr.Start() call at the end of main()). This enables fast failover during
		// deployments and upgrades, reducing downtime from ~60s to ~1-2s.
		LeaderElectionReleaseOnCancel: true,
	}

	watchNS := cfg.WatchNamespace()
	if watchNS != "" {
		setupLog.Info("Watching single namespace", "namespace", watchNS)
		namespaces := map[string]cache.Config{
			watchNS: {},
		}
		// The policy namespace is deliberately NOT added to the cache.
		//
		// Adding it starts an informer, and an informer that cannot list its
		// namespace never syncs — so WaitForCacheSync blocks forever and the
		// controller never becomes ready. That is the normal case here, not an edge
		// one: a namespace-scoped install holds RBAC in its own namespace only, and
		// the whole point of the policy namespace is that it belongs to someone
		// else. An admin applying the documented policy-namespace label would have
		// bricked the very controller they were trying to bound.
		//
		// Cluster policy is read through the direct API reader instead (the
		// ConfigMapReconciler holds mgr.GetAPIReader()), which fails as a plain
		// error we can report rather than as a hang.
		//
		// The cost is that a policy edit in that namespace is no longer watched, so
		// it takes effect on restart rather than live. That is the right trade: a
		// bound that updates slowly beats a controller that never starts, and a
		// namespace-scoped install cannot watch that namespace anyway without RBAC
		// it does not have.
		mgrOptions.Cache = cache.Options{
			DefaultNamespaces: namespaces,
		}
	} else {
		// Multi-namespace mode: Use label selector to filter ConfigMaps in the cache
		// This significantly reduces memory usage by only caching WVA-related configmaps
		wvaConfigSelector := labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/name": "workload-variant-autoscaler",
		})

		setupLog.Info("Configuring cache with label selector for ConfigMaps",
			"labelSelector", wvaConfigSelector.String())

		// Configure cache to only watch configmaps with the WVA labels
		// Other resource types are cached normally without filtering
		cmNamespaces := map[string]cache.Config{}
		if cfg.PolicyNamespaceIsSeparate() {
			// Exempt the policy namespace from the label filter. That ConfigMap is
			// written by a cluster admin, by hand or by their own tooling, and
			// expecting them to reproduce one of our labels turns a forgotten label
			// into a controller that reads no quota and says nothing. The bound has
			// to survive being authored by someone who has not read our source.
			// labels.Everything() explicitly: a nil LabelSelector is DEFAULTED to
			// ByObject.Label, so leaving it unset would silently reapply the very
			// filter this entry exists to lift.
			policyNS := cfg.PolicyNamespace()
			cmNamespaces[cache.AllNamespaces] = cache.Config{LabelSelector: wvaConfigSelector}
			cmNamespaces[policyNS] = cache.Config{LabelSelector: labels.Everything()}
			setupLog.Info("Caching the cluster policy namespace without the label filter",
				"policyNamespace", policyNS)
		}
		mgrOptions.Cache = cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.ConfigMap{}: {
					// Empty map means cache all namespaces, but filter by label
					Namespaces: cmNamespaces,
					Label:      wvaConfigSelector,
				},
			},
		}
	}

	mgr, err := ctrl.NewManager(restConfig, mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Initialize metrics
	setupLog.Info("Creating metrics emitter instance")
	// Force initialization of metrics by creating a metrics emitter
	_ = metrics.NewMetricsEmitter()
	setupLog.Info("Metrics emitter created successfully")

	// Create ConfigMap reconciler for configuration management.
	// Bootstrap uses the temporary uncached client so ConfigMap-backed settings
	// are loaded before any manager runnables start.
	configMapReconciler := &controller.ConfigMapReconciler{
		Reader:    mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Config:    cfg,
		Datastore: ds,
		Recorder:  mgr.GetEventRecorderFor("workload-variant-autoscaler-configmap-reconciler"),
	}

	ctx := context.Background()
	ctx = ctrl.LoggerInto(ctx, setupLog)
	if err = configMapReconciler.BootstrapInitialConfigMaps(ctx); err != nil {
		setupLog.Error(err, "unable to bootstrap initial ConfigMaps")
		os.Exit(1)
	}
	setupLog.Info("Initial ConfigMap bootstrap completed")

	// Frozen registration decision: analyzer registration cannot change without
	// a controller restart, so this value is captured once (after the bootstrap
	// above has loaded ConfigMap-backed settings) and shared with the
	// ConfigMapReconciler so it can detect a later live-config divergence.
	taRegistered := cfg.ThroughputAnalyzerEnabled()
	configMapReconciler.ThroughputRegistered = taRegistered

	// Use Prometheus configuration from unified Config (already validated during Load())
	if cfg.PrometheusBaseURL() == "" {
		setupLog.Error(nil, "no Prometheus configuration found - this should not happen after validation")
		os.Exit(1)
	}

	// Validate Prometheus transport configuration before creating the client.
	if err := prometheusutil.ValidateTLSConfig(cfg); err != nil {
		setupLog.Error(err, "Prometheus transport configuration validation failed")
		os.Exit(1)
	}

	promURL, _ := url.Parse(cfg.PrometheusBaseURL()) // already validated above
	setupLog.Info("Initializing Prometheus client",
		"address", promURL.Redacted(),
		"tlsEnabled", prometheusutil.IsHTTPS(cfg.PrometheusBaseURL()),
		"allowHTTP", cfg.PrometheusAllowHTTP(),
	)

	// Create Prometheus client with TLS support
	promClientConfig, err := prometheusutil.CreatePrometheusClientConfig(cfg)
	if err != nil {
		setupLog.Error(err, "failed to create prometheus client config")
		os.Exit(1)
	}

	promClient, err := api.NewClient(*promClientConfig)
	if err != nil {
		setupLog.Error(err, "failed to create prometheus client")
		os.Exit(1)
	}

	promAPI := promv1.NewAPI(promClient)

	// Validate that the API is working by testing a simple query with retry logic
	if err := prometheusutil.ValidatePrometheusAPI(context.Background(), promAPI); err != nil {
		setupLog.Error(err, "CRITICAL: Failed to connect to Prometheus - WVA requires Prometheus connectivity for autoscaling decisions")
		os.Exit(1)
	}
	setupLog.Info("Prometheus client and API wrapper initialized and validated successfully")

	// Keep the cluster's GPU usage picture current, independently of either engine.
	//
	// Registered BEFORE them so it takes its first observation first: both engines
	// consume this, and until it exists the capacity checks have no evidence and
	// degrade to "unknown", which is permissive. It is the SOLE producer of the
	// physical snapshot — see internal/gpuusage.
	//
	// The TIMER is only worth running for a deployment that reads the physical
	// view, which is exactly one condition now that the limiters list is the sole
	// source of truth: a physical limiter is declared. Nothing else consumes this.
	// A deployment that declares no limiter is not limited at all, and one that
	// declares only a quota is charged for WVA's own variants
	// (allocation.ManagedUsage) — both would otherwise list nodes and walk every pod
	// in the cluster every interval to produce a number with no reader.
	//
	// Re-read every tick, because the limiters list is live: switching it must not
	// need a restart.
	usageRefresher := &gpuusage.Refresher{
		Discovery: gpunodes.NewK8sWithGpuOperator(mgr.GetClient()),
		Periodic:  func() bool { return allocation.PhysicalUsageConfigured(cfg) },
	}
	if err := mgr.Add(usageRefresher); err != nil {
		setupLog.Error(err, "unable to add the GPU usage refresher to the manager")
		os.Exit(1)
	}

	// Register optimization engine loops with the manager. Only start when leader.
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		sourceRegistry := source.NewSourceRegistry()
		setupLog.Info("Initializing metrics source registry")

		// Prometheus cache configuration is loaded via unified Config during startup.
		// The cache config is available in cfg.Dynamic.PrometheusCache and is updated
		// automatically when the ConfigMap changes. We use the default config here
		// as the unified Config system handles cache configuration loading.

		// Register PrometheusSource with default config
		promSource := prometheus.NewPrometheusSource(ctx, promAPI, prometheus.DefaultPrometheusSourceConfig())

		// Register in global source registry
		if err := sourceRegistry.Register("prometheus", promSource); err != nil {
			setupLog.Error(err, "failed to register prometheus source in source registry")
			os.Exit(1)
		}

		// Build the initial GPU limiter from the effective limiter mode — the
		// limiters: list on the saturation "default" config, which is the sole
		// source. The ConfigMaps were bootstrapped above, so the selection is
		// already visible here. The engine rebuilds the limiter live (see
		// SetLimiterBuilder) when the ConfigMap changes.
		gpuLimiter, err := allocation.NewLimiterFromConfig(cfg, mgr.GetClient())
		if err != nil {
			setupLog.Error(err, "failed to build GPU limiter")
			return err
		}
		setupLog.Info("GPU limiter constructed", "type", cfg.EffectiveLimiterMode(), "name", gpuLimiter.Name())

		// Declaring no limiter is a legitimate configuration, but it is worth saying
		// out loud: it means scaling is bounded by nothing WVA knows about. The
		// optimizer allocates without a GPU budget and a scale-from-zero wake is
		// published without a capacity check, so a variant can be woken onto an
		// accelerator that is already full. There is no longer a hidden default
		// limiter making this safe.
		if cfg.EffectiveLimiterMode() == config.LimiterTypeNone {
			setupLog.Info("No limiters declared in the saturation-scaling ConfigMap: scaling is " +
				"UNCONSTRAINED. The optimizer allocates without a GPU budget and scale-from-zero " +
				"wakes are published without a capacity check. Add a limiters: entry " +
				"(gpu-inventory or quota) to bound scaling.")
		}

		// A limiters: list with more than one kind reads as several bounds that all
		// apply. Only one is built, quota wins, and the physical one is dropped —
		// so the fleet is bounded by the declared allowance while nothing checks
		// whether the GPUs exist. Said at startup as well as at ConfigMap parse,
		// because this is where an operator looks to see what is limiting them.
		if dropped := cfg.UnenforcedLimiterTypes(); len(dropped) > 0 {
			setupLog.Info("WARNING: declared limiters that are NOT enforced: a quota entry selects the "+
				"quota limiter, and it is the only one built. Physical GPU capacity is not "+
				"consulted, so scaling is bounded by the declared allowance alone. Declare one "+
				"kind, or track issue #1003 for bounding by min(physical, quota)",
				"notEnforced", dropped, "enforcing", cfg.EffectiveLimiterMode())
		}

		// Quota mode means "no physical-capacity discovery" — including the
		// inventory-collection call in the saturation engine. We honor that
		// at the call site (see steadystate.shouldCollectClusterInventory),
		// but warn loudly here so an operator who explicitly enabled
		// WVA_LIMITED_MODE sees that their inventory log will be suppressed.
		if cfg.EffectiveLimiterMode() == config.LimiterTypeQuota && cfg.LimitedModeEnabled() {
			setupLog.Info("Quota limiter mode is active; cluster inventory collection is disabled "+
				"despite WVA_LIMITED_MODE=true (no Node API access in quota mode). "+
				"To re-enable cluster inventory logging, switch to inventory mode.",
				"limiterType", cfg.EffectiveLimiterMode(),
				"limitedModeEnabled", cfg.LimitedModeEnabled())
		}

		engine := steadystate.NewEngine(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			mgr.GetScheme(),
			mgr.GetEventRecorderFor("workload-variant-autoscaler-saturation-engine"),
			sourceRegistry,
			cfg, // Pass unified Config to engine
			gpuLimiter,
		)
		// Discovery: the workloads KEDA has called the external scaler about.
		// The enricher reads with GetAPIReader deliberately — a cached read of a
		// ScaledObject is served by a cluster-wide informer, which is the LIST+WATCH
		// this design exists to remove.
		engine.Variants = registry.Default
		engine.VariantEnricher = registry.NewEnricher(mgr.GetAPIReader(), registry.Default, registry.DefaultTargetMaxAge)
		// The registry is also what tells the datastore which namespaces to load
		// configuration for. That used to come from a ScaledObject reconciler, whose
		// watch was a cluster-wide informer; the registry knows the same thing for a
		// better reason — these are the namespaces WVA has actually been called about.
		engine.VariantEnricher.Tracker = ds
		// Rebuild the limiter live when the saturation ConfigMap's limiters: list
		// changes — no restart required. The builder re-reads the effective config.
		engine.SetLimiterBuilder(func() (allocation.Limiter, error) {
			return allocation.NewLimiterFromConfig(cfg, mgr.GetClient())
		})
		// Arrival rate is registered whatever the analyzer set: the saturation
		// analyzer's demand floor needs it too, and gating it on the throughput
		// analyzer made that floor structurally inoperable whenever throughput
		// was disabled -- which is the default.
		registration.RegisterArrivalRateQueries(sourceRegistry)
		if taRegistered {
			registration.RegisterThroughputAnalyzerQueries(sourceRegistry)
			if err := engine.RegisterAnalyzer(throughput.AnalyzerName, throughput.NewThroughputAnalyzer()); err != nil {
				return err
			}
			setupLog.Info("ThroughputAnalyzer registered (enabled in saturation config)")
		} else {
			setupLog.Info("ThroughputAnalyzer NOT registered — no saturation config entry " +
				"enables 'throughput'. Add it to the analyzers config and restart the " +
				"controller to enable it.")
		}
		go engine.StartOptimizeLoop(ctx)
		return nil
	}))

	if err != nil {
		setupLog.Error(err, "unable to add optimization engine loop to manager")
		os.Exit(1)
	}

	// Register scale from zero engine loop with the manager. Only start when leader.
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		engine, err := scalefromzero.NewEngine(mgr.GetClient(), mgr.GetEventRecorderFor("workload-variant-autoscaler-scalezero-engine"), ds, cfg)
		if err != nil {
			return err
		}
		// Same registry as the saturation engine: one call registers a workload for
		// both. Its own Enricher, because the two loops run at different rates and
		// each must be free to refresh when it needs to — they share the registry,
		// which is where the freshness actually lives, so neither re-reads what the
		// other has just read.
		engine.Variants = registry.Default
		engine.VariantEnricher = registry.NewEnricher(mgr.GetAPIReader(), registry.Default, registry.DefaultTargetMaxAge)
		// Let the engine bring the usage picture up to date immediately before it
		// places a wake. The periodic observation alone can be a full interval old
		// at the moment it is consulted, and a workload that has just started
		// holding GPUs is exactly what a placement must not miss.
		engine.UsageRefresher = usageRefresher
		// Second transport for the wake signal. This engine scrapes the EPP pod
		// directly, which is the only WVA metric path that does not go through
		// Prometheus — so a broken token, missing EPP tokenreview RBAC or a
		// NetworkPolicy takes out wakes alone, silently, while the same queue
		// depth sits readable in Prometheus. Nil source yields a nil fallback and
		// the pre-existing behaviour; see queue_fallback.go.
		engine.QueueFallback = scalefromzero.NewQueueFallback(
			prometheus.NewPrometheusSource(ctx, promAPI, prometheus.DefaultPrometheusSourceConfig()))
		// Give the engine a limiter so a wake is only published for a variant
		// that can actually be placed. This is its own instance rather than the
		// saturation engine's: a limiter supplies constraints from usage passed
		// per call, so instances share no mutable state, and building one here
		// avoids threading the other engine's closure-scoped value across.
		//
		// Rebuilt live on a ConfigMap change, exactly like the saturation engine's.
		// It used to be built once here and never again, so editing the limiters:
		// list changed how the optimizer allocated and left the scale-from-zero
		// capacity check running against whatever was configured at startup — with
		// the list now the sole switch for both, that split is a trap. A limiter
		// that cannot be built is not fatal: the engine then wakes without a
		// capacity check, which is what it did before selection existed.
		if sfzLimiter, limErr := allocation.NewLimiterFromConfig(cfg, mgr.GetClient()); limErr != nil {
			setupLog.Error(limErr, "failed to build GPU limiter for scale-from-zero; waking without a capacity check")
		} else {
			engine.SetGPULimiter(sfzLimiter)
		}
		engine.SetLimiterBuilder(func() (allocation.Limiter, error) {
			return allocation.NewLimiterFromConfig(cfg, mgr.GetClient())
		})
		go engine.StartOptimizeLoop(ctx)
		return nil
	}))

	if err != nil {
		setupLog.Error(err, "unable to add optimization engine loop to manager")
		os.Exit(1)
	}

	// Register the KEDA external scaler gRPC server. Leader-gated (a plain
	// manager.Runnable) so it is co-located with the optimize loop that feeds the
	// in-memory decision store it serves.
	//
	// It writes registry.Default as well as reading the decision store: every call
	// registers the workload it names, which is how WVA discovers what it manages
	// (docs/plans/engine/keda-driven-discovery.md). Leader-gating it therefore also
	// gates discovery — correct, since only the leader runs the engines that
	// consume the registry.
	if err := mgr.Add(&scaler.Server{
		Addr:     *externalScalerBindAddress,
		Client:   mgr.GetAPIReader(),
		Registry: registry.Default,
	}); err != nil {
		setupLog.Error(err, "unable to add KEDA external scaler to manager")
		os.Exit(1)
	}

	// The warm pool: GPU-holding Pods that keep models resident but asleep, so a
	// scale-up can be covered in ~0.4 s while the ordinary replicas take ~35 s.
	//
	// Leader-gated with everything else, and driven off the decision store rather
	// than off KEDA: a decision is known here before KEDA is told about it, so a
	// bridge starts at decision time instead of a poll interval later. The pool
	// never touches the metric KEDA reads -- it borrows underneath the same
	// decision KEDA is about to act on, which is what keeps lent capacity out of
	// the scaling arithmetic.
	if *warmPoolNamespace != "" {
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			trigger := warmpool.NewDecisionTrigger(decision.Default, nil)
			defer trigger.Close()
			// Discovery is call-driven, so the set of scale targets grows during
			// a run; a trigger built once at startup would never fire for a
			// workload registered later.
			go trigger.WatchRegistry(ctx, registry.Default, 30*time.Second)

			reconciler := warmpool.New(
				warmpoolpool.NewAdapter(mgr.GetClient(), *warmPoolNamespace, warmpoolpool.Ram),
				&warmpool.Demand{
					Registry:  registry.Default,
					Decisions: decision.Default,
					Client:    mgr.GetClient(),
					Datastore: ds,
				},
				warmpoolpolicy.Config{
					SleepMinSize:       *warmPoolSleepMinSize,
					MaxHold:            *warmPoolMaxHold,
					AdmissionWindow:    time.Hour,
					MinMissesToAdmit:   2,
					MaxInstancesPerPod: warmpoolpool.MaxInstancesPerPod,
				},
			)
			reconciler.Name = *warmPoolNamespace
			reconciler.Trigger = trigger
			setupLog.Info("warm pool enabled",
				"namespace", *warmPoolNamespace,
				"sleepMinSize", *warmPoolSleepMinSize,
				"maxHold", *warmPoolMaxHold)
			return reconciler.Start(ctx)
		})); err != nil {
			setupLog.Error(err, "unable to add the warm pool to manager")
			os.Exit(1)
		}
	}

	// +kubebuilder:scaffold:builder

	// Create InferencePool reconciler
	poolGroupEnv := os.Getenv("POOL_GROUP")
	poolGKNN, err := poolutil.GetPoolGKNN(poolGroupEnv)
	if err != nil {
		setupLog.Error(err, "unable to create default pool GKNN from POOL_GROUP", "poolGroup", poolGroupEnv)
		os.Exit(1)
	}
	inferencePoolReconciler := &controller.InferencePoolReconciler{
		Datastore: ds,
		Client:    mgr.GetClient(),
		PoolGKNN:  poolGKNN,
	}

	if err = inferencePoolReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create inferencePool controller")
		os.Exit(1)
	}

	if err = configMapReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create configmap controller")
		os.Exit(1)
	}

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// Cluster policy this controller cannot serve takes it OUT OF SERVICE, rather
	// than leaving it running and silently refusing to scale anything.
	//
	// It is a readiness check, not a startup check, because the failure has two
	// arrival times and only one of them is an install. On day one it fails the
	// rollout, so `verify_deployment` reports a failed install instead of a green
	// one. On day ninety — when a cluster admin adds a gpu-inventory limiter to
	// cluster policy, which this controller reloads live — the same condition
	// becomes true and the pod goes NotReady, which is visible, alertable, and
	// nothing like the alternative: every variant quietly charged to no accelerator
	// pool, given no budget, and never scaling up again.
	if err := mgr.AddReadyzCheck("gpu-policy", func(req *http.Request) error {
		if cfg.EffectiveLimiterMode() != config.LimiterTypeInventory {
			return nil
		}
		// Ask the API server, rather than reading the flag the optimization loop
		// sets. Reading the flag made this a ONE-WAY LATCH, and the loop that would
		// have cleared it runs downstream of the very thing going NotReady breaks:
		// no endpoints on the external-scaler Service, so KEDA stops calling, so the
		// registry empties on its TTL, so the optimizer returns early every cycle
		// and never retries node discovery. Granting the RBAC — which is exactly
		// what the error below tells an admin to do — would not have recovered it.
		// Only deleting the pod would, and a transient Forbidden during RBAC
		// propagation would have parked the controller permanently.
		if err := nodeReadProbe(req.Context(), policyProbe); err != nil {
			return fmt.Errorf("cluster policy declares a gpu-inventory limiter but this controller may not list nodes: %w. "+
				"Every variant would be charged to no accelerator pool, receive no GPU budget and never scale up. "+
				"Grant its ServiceAccount get/list/watch on nodes, or remove the limiter from cluster policy", err)
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "unable to set up the GPU policy ready check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
		if cfg.ConfigMapsBootstrapComplete() {
			return nil
		}
		_, _, syncErr := cfg.ConfigMapsBootstrapSyncStatus()
		if syncErr != "" {
			return fmt.Errorf("initial ConfigMap bootstrap not complete: %s", syncErr)
		}
		return errors.New("initial ConfigMap bootstrap not complete")
	}); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")

	// Sync the custom logger before starting the manager
	if err := logging.Sync(); err != nil {
		setupLog.Error(err, "Failed to sync logger before starting manager")
		os.Exit(1)
	}

	// Register custom metrics with the controller-runtime Prometheus registry
	// This makes the metrics available for scraping by Prometheus and direct endpoint access
	setupLog.Info("Registering custom metrics with Prometheus registry")
	if err := metrics.InitMetrics(crmetrics.Registry); err != nil {
		setupLog.Error(err, "failed to initialize metrics")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// nodeReadProbeCache rate-limits the readiness gate's node check. A readiness
// probe fires every few seconds on every replica; an unlimited List of nodes on
// that schedule is load the API server does not need to carry.
var (
	nodeReadProbeMu   sync.Mutex
	nodeReadProbeAt   time.Time
	nodeReadProbeErr  error
	nodeReadProbeTTL  = 30 * time.Second
	nodeReadProbeOnce bool
)

// nodeReadProbe reports whether this controller can currently list nodes.
//
// It asks the API server rather than trusting a cached verdict from elsewhere in
// the process, so that granting the RBAC recovers readiness on its own. `Limit: 1`
// keeps it cheap: the question is whether the request is authorized, not what the
// nodes contain.
func nodeReadProbe(ctx context.Context, c client.Reader) error {
	nodeReadProbeMu.Lock()
	defer nodeReadProbeMu.Unlock()
	if nodeReadProbeOnce && time.Since(nodeReadProbeAt) < nodeReadProbeTTL {
		return nodeReadProbeErr
	}
	nodes := &corev1.NodeList{}
	err := c.List(ctx, nodes, client.Limit(1))
	nodeReadProbeAt, nodeReadProbeErr, nodeReadProbeOnce = time.Now(), err, true
	return err
}
