// Command kubectl-ravendb_cloud is a kubectl plugin: on the PATH, it runs as
// "kubectl ravendb-cloud". Its one subcommand, export, prints CloudCluster
// manifests for clusters that already run in RavenDB Cloud, so they can be
// taken over without being reshaped.
//
//	kubectl ravendb-cloud export --account ravendb-cloud -n apps orders billing > clusters.yaml
//	kubectl ravendb-cloud export --account ravendb-cloud -n apps --all > clusters.yaml
//
// The API key comes from the CloudAccount's own Secret, read with the
// kubeconfig, so it never has to be on the laptop. RAVENDB_CLOUD_API_KEY
// overrides that, for an account that is not applied yet.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
	"github.com/ravendb/ravendb-cloud-operator/internal/export"
)

// apiKeyEnvVar holds a key to use instead of the account's Secret. Not a flag,
// so it stays out of shell history and process listings.
const apiKeyEnvVar = "RAVENDB_CLOUD_API_KEY"

const userAgent = "kubectl-ravendb-cloud"

const usage = "kubectl ravendb-cloud export --account NAME [-n NAMESPACE] (--all | DISPLAY-NAME...)"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, connectWithKubeconfig); err != nil {
		fmt.Fprintln(os.Stderr, "kubectl ravendb-cloud:", err)
		os.Exit(1)
	}
}

// connect returns a client for the cluster the kubeconfig names, and the
// namespace it selects.
type connect func() (client.Client, string, error)

func run(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer, connect connect) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New("usage: " + usage)
	}

	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.Usage = func() {
		out := flags.Output()
		fmt.Fprintf(out, "Usage: %s\n\n", usage)
		fmt.Fprintln(out, "Prints a CloudCluster manifest for each named RavenDB Cloud cluster, matching what runs,")
		fmt.Fprintln(out, "so applying it takes the cluster over without changing it. The API key comes from the")
		fmt.Fprintf(out, "CloudAccount's Secret, or from %s when that is set.\n\n", apiKeyEnvVar)
		flags.PrintDefaults()
	}
	account := flags.String("account", "", "the CloudAccount to read the key from and to put in spec.accountRef (required)")
	var namespace string
	flags.StringVar(&namespace, "namespace", "", "the namespace of the CloudAccount and the manifests; the kubeconfig's when empty")
	flags.StringVar(&namespace, "n", "", "shorthand for --namespace")
	all := flags.Bool("all", false, "export every cluster in the account that has not been terminated")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	names := flags.Args()
	switch {
	case *account == "":
		return errors.New("--account is required: it names the CloudAccount the manifests will use")
	case *all && len(names) > 0:
		return errors.New("give display names or --all, not both")
	case !*all && len(names) == 0:
		return errors.New("give the display names of the clusters to export, or --all")
	}

	cloud, namespace, err := cloudFor(ctx, *account, namespace, getenv, connect)
	if err != nil {
		return err
	}

	to := export.Target{Account: *account, Namespace: namespace}
	var out []byte
	if *all {
		out, err = export.All(ctx, cloud, to)
	} else {
		out, err = export.Named(ctx, cloud, to, names)
	}
	if err != nil {
		return err
	}
	_, err = stdout.Write(out)
	return err
}

// cloudFor builds the client the export reads through, and settles the
// namespace the manifests go in.
func cloudFor(
	ctx context.Context, account, namespace string, getenv func(string) string, connect connect,
) (cloudapi.Client, string, error) {
	if key := strings.TrimSpace(getenv(apiKeyEnvVar)); key != "" {
		cloud, err := cloudapi.NewRESTClient(cloudapi.Options{
			APIKey:    key,
			Endpoint:  getenv(cloudapi.EndpointEnvVar),
			UserAgent: userAgent,
		})
		return cloud, namespace, err
	}

	kube, current, err := connect()
	if err != nil {
		return nil, "", fmt.Errorf("reading CloudAccount %q needs the cluster it is in: %w; set %s to use a key directly",
			account, err, apiKeyEnvVar)
	}
	// The manifests name the account in spec.accountRef, which only reaches
	// their own namespace, so they go where the account is.
	namespace = cmp.Or(namespace, current)
	var found v1alpha1.CloudAccount
	err = kube.Get(ctx, types.NamespacedName{Namespace: namespace, Name: account}, &found)
	switch {
	case apierrors.IsNotFound(err):
		return nil, "", fmt.Errorf("CloudAccount %q not found in namespace %q: give its namespace with -n, or set %s to use a key directly",
			account, namespace, apiKeyEnvVar)
	case err != nil:
		return nil, "", fmt.Errorf("reading CloudAccount %q: %w", account, err)
	}

	clients := cloudclients.NewCache(kube, userAgent)
	clients.Endpoint = getenv(cloudapi.EndpointEnvVar)
	cloud, err := clients.ClientFor(ctx, &found)
	if err != nil {
		return nil, "", fmt.Errorf("CloudAccount %q: %w", account, err)
	}
	return cloud, namespace, nil
}

// connectWithKubeconfig reads the kubeconfig the way kubectl does: KUBECONFIG,
// then ~/.kube/config, then the in-cluster service account.
func connectWithKubeconfig() (client.Client, string, error) {
	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		return nil, "", err
	}
	namespace, _, err := kubeconfig.Namespace()
	if err != nil {
		return nil, "", err
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	return kube, namespace, err
}
