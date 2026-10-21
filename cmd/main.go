// Command manager runs the RavenDB Cloud operator.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
	"github.com/ravendb/ravendb-cloud-operator/internal/controller"
	"github.com/ravendb/ravendb-cloud-operator/internal/version"
)

// userAgent names this operator and its version on every request to RavenDB
// Cloud.
var userAgent = "ravendb-cloud-operator/" + version.Version

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// gracefulShutdown is how long the manager waits for in-flight reconciles
	// to return after SIGTERM. Their context is cancelled at once, so a call
	// to RavenDB Cloud in flight is abandoned, but the status write that ends
	// a pass has its own short deadline and is worth waiting for.
	gracefulShutdown = 30 * time.Second
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

// main only turns an error into an exit code, so everything below can fail by
// returning.
func main() {
	if err := run(); err != nil {
		setupLog.Error(err, "operator stopped")
		os.Exit(1)
	}
}

// config is everything the operator takes from its command line.
type config struct {
	metricsAddr    string
	probeAddr      string
	leaderElection bool
	secureMetrics  bool
	watchNamespace string
	printVersion   bool
}

// bootstrap declares every flag, parses the command line, and installs the
// logger, in that order. zap's flags have to be registered before flag.Parse,
// or Parse rejects -zap-devel as undefined, and the logger has to be built
// after it, or every -zap-* flag is silently ignored.
func bootstrap() config {
	var cfg config

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0",
		"Address the metrics endpoint binds to. Use 0 to disable.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081",
		"Address the probe endpoint binds to.")
	// Off by default so a manager started by hand, outside the cluster, needs
	// no lease permissions. The deployment has to turn it on: the pacing is
	// per process, so two active managers would each call at the full pace.
	flag.BoolVar(&cfg.leaderElection, "leader-elect", false,
		"Enable leader election, so only one manager is active at a time.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"Serve metrics over HTTPS, and require a caller to authenticate.")
	flag.StringVar(&cfg.watchNamespace, "watch-namespace", "",
		"Restrict the operator to one namespace. Empty means all namespaces.")
	flag.BoolVar(&cfg.printVersion, "version", false, "Print the version and exit.")

	logOpts := logDefaults()
	logOpts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOpts)))
	return cfg
}

// logDefaults is production logging, JSON, sampled, info and above, with no
// stack trace on an error line. In practice every error line is
// controller-runtime's "Reconciler error", and its stack trace is always the
// same handler, which says nothing about why RavenDB Cloud did not answer.
// -zap-stacktrace-level=error brings the traces back.
func logDefaults() zap.Options {
	return zap.Options{StacktraceLevel: zapcore.PanicLevel}
}

// run builds the manager and blocks until it is signalled to stop. A nil
// return means a clean shutdown.
func run() error {
	cfg := bootstrap()
	if cfg.printVersion {
		fmt.Println(version.Version)
		return nil
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions(cfg))
	if err != nil {
		return fmt.Errorf("building the manager: %w", err)
	}

	// One Cache for every controller: it holds the one pace every call in the
	// process shares, and one each would multiply it.
	clients := cloudclients.NewCache(mgr.GetClient(), userAgent)
	clients.Endpoint = os.Getenv(cloudapi.EndpointEnvVar)
	clients.Pacing.RateLimit = rateLimitFrom(os.Getenv(rateLimitEnvVar))

	if err := addControllers(mgr, clients); err != nil {
		return err
	}
	ctrlmetrics.Registry.MustRegister(cloudapi.Collectors()...)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding the health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding the ready check: %w", err)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("running the manager: %w", err)
	}
	return nil
}

// managerOptions is every knob the manager is built with, in one function so
// that a test can read them back without a cluster to start against.
func managerOptions(cfg config) ctrl.Options {
	opts := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions(cfg),
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElection,
		LeaderElectionID:       "ravendb-cloud-operator.cloud.ravendb.net",

		// Hand the lease back on a clean shutdown instead of leaving the
		// operator leaderless until it expires. Safe because Start returning is
		// the last thing this process does.
		LeaderElectionReleaseOnCancel: true,

		// Inside terminationGracePeriodSeconds, which both deployment manifests
		// set to 40, or the kubelet's SIGKILL arrives first and this decides
		// nothing.
		GracefulShutdownTimeout: &gracefulShutdown,

		// Both controllers watch Secrets as metadata only, so no payload ever
		// reaches this process through a watch. The two reads that need one
		// go straight to the API server instead of starting a full informer,
		// which would hold every Secret in the cluster, contents included.
		Client: client.Options{
			Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}},
		},
	}
	if cfg.watchNamespace != "" {
		opts.Cache.DefaultNamespaces = map[string]cache.Config{cfg.watchNamespace: {}}
	}
	return opts
}

func metricsOptions(cfg config) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
	}
	if cfg.secureMetrics {
		// TLS only encrypts. Without this filter anyone who can reach the port,
		// on a flat pod network the whole cluster, could read every metric.
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
		opts.TLSOpts = append(opts.TLSOpts, serveHTTP11Only)
	}
	return opts
}

// serveHTTP11Only takes HTTP/2 off the metrics listener, which removes
// CVE-2023-44487 and CVE-2023-39325 (streams opened and cancelled faster than
// the server retires them). Prometheus does not need HTTP/2 to scrape, and a
// plaintext listener never negotiates it.
func serveHTTP11Only(c *tls.Config) {
	c.NextProtos = []string{"http/1.1"}
}

// rateLimitEnvVar overrides the pacing, in requests per second. It is for the
// simulator, where production pacing is pure delay.
const rateLimitEnvVar = "RAVENDB_CLOUD_RATE_LIMIT_PER_SECOND"

// rateLimitFrom reads rateLimitEnvVar. Anything but a positive number keeps
// production pacing, rather than being read as zero, which would mean "never
// send anything".
func rateLimitFrom(value string) rate.Limit {
	perSecond, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || perSecond <= 0 {
		return 0
	}
	return rate.Limit(perSecond)
}

// addControllers is the list of things this operator reconciles. A new
// controller goes here and nowhere else.
func addControllers(mgr ctrl.Manager, clients *cloudclients.Cache) error {
	if err := (&controller.CloudAccountReconciler{
		Client:   mgr.GetClient(),
		Clients:  clients,
		Recorder: mgr.GetEventRecorder("cloudaccount-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the CloudAccount controller: %w", err)
	}

	if err := (&controller.CloudClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Clients:  clients,
		Recorder: mgr.GetEventRecorder("cloudcluster-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the CloudCluster controller: %w", err)
	}

	if err := (&controller.CloudClusterRestartReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Clients:  clients,
		Recorder: mgr.GetEventRecorder("cloudclusterrestart-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up the CloudClusterRestart controller: %w", err)
	}

	return nil
}
