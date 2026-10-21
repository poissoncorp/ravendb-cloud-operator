package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// Secret reads bypass the cache: the only Secret informer is metadata only,
// and a typed read through the cache would start a second one holding every
// payload in the cluster. Narrowing to a namespace must keep that, since the
// namespace list and the client options are separate fields set in separate
// places.
func TestManagerOptionsReadSecretsLive(t *testing.T) {
	for _, cfg := range []config{{}, {watchNamespace: "apps"}} {
		opts := managerOptions(cfg)
		if !readsSecretsLive(opts) {
			t.Errorf("watchNamespace %q: Secret reads go through the cache", cfg.watchNamespace)
		}
	}
	if _, ok := managerOptions(config{watchNamespace: "apps"}).Cache.DefaultNamespaces["apps"]; !ok {
		t.Error("watch-namespace did not reach the cache")
	}
}

func readsSecretsLive(opts ctrl.Options) bool {
	if opts.Client.Cache == nil {
		return false
	}
	for _, obj := range opts.Client.Cache.DisableFor {
		if _, ok := obj.(*corev1.Secret); ok {
			return true
		}
	}
	return false
}

// The metrics listener negotiates HTTP/2 by default, which is the protocol
// CVE-2023-44487 abuses. Nothing else in the process observes the difference,
// so only reading the option back catches it going missing.
func TestManagerOptionsServeMetricsOverHTTP11Only(t *testing.T) {
	opts := managerOptions(config{metricsAddr: ":8443", secureMetrics: true})

	if len(opts.Metrics.TLSOpts) == 0 {
		t.Fatal("no TLS options on the metrics server, so HTTP/2 is still offered")
	}
	var tlsCfg tls.Config
	for _, apply := range opts.Metrics.TLSOpts {
		apply(&tlsCfg)
	}
	if !slices.Equal(tlsCfg.NextProtos, []string{"http/1.1"}) {
		t.Errorf("NextProtos is %v, want only http/1.1", tlsCfg.NextProtos)
	}
}

// Plaintext metrics never negotiate HTTP/2, and the authentication filter is
// what the TLS options travel with, so neither is set when serving is off.
func TestManagerOptionsLeaveAPlaintextMetricsListenerAlone(t *testing.T) {
	opts := managerOptions(config{metricsAddr: ":8080", secureMetrics: false})

	if len(opts.Metrics.TLSOpts) != 0 {
		t.Errorf("TLS options on a plaintext listener: %d", len(opts.Metrics.TLSOpts))
	}
	if opts.Metrics.FilterProvider != nil {
		t.Error("an authentication filter on a plaintext listener")
	}
}

// TestOnlyAPositivePaceOverridesProduction: zero is production pacing in
// cloudclients.Pacing, so a typo must land there and not on a pace that never
// sends anything.
func TestOnlyAPositivePaceOverridesProduction(t *testing.T) {
	for value, want := range map[string]rate.Limit{
		"50": 50, " 2.5 ": 2.5, "": 0, "0": 0, "-1": 0, "fast": 0,
	} {
		if got := rateLimitFrom(value); got != want {
			t.Errorf("rateLimitFrom(%q) = %v, want %v", value, got, want)
		}
	}
}

// TestAnErrorLineCarriesNoStackTrace pins the one change logDefaults makes to
// production logging. Every error line an outage produced was the same
// "Reconciler error" with the same stack trace through controller-runtime's own
// handler, which was most of the bytes and none of the information.
func TestAnErrorLineCarriesNoStackTrace(t *testing.T) {
	var out bytes.Buffer
	opts := logDefaults()
	logger := zap.New(zap.UseFlagOptions(&opts), zap.WriteTo(&out))

	logger.Error(errors.New("dial tcp 10.96.48.1:80: connect: connection refused"), "Reconciler error")

	logged := out.String()
	if !strings.Contains(logged, "connection refused") {
		t.Fatalf("the error itself was not logged: %q", logged)
	}
	if strings.Contains(logged, "stacktrace") {
		t.Errorf("an error line carries a stack trace by default: %q", logged)
	}
}
