package cloudapi

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// These describe every call any client in the process makes. They are package
// state because the clients share one connection pool and one pace; the
// operator registers them where its metrics endpoint serves them.
var (
	attempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ravendb_cloud_requests_total",
		Help: "Requests sent to RavenDB Cloud, each attempt counted, by method and " +
			"HTTP status code, or \"none\" when no answer came.",
	}, []string{"method", "code"})

	attemptSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ravendb_cloud_request_duration_seconds",
		Help:    "How long one attempt took, by method and HTTP status code.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"method", "code"})

	retries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ravendb_cloud_retries_total",
		Help: "Attempts after the first, by method.",
	}, []string{"method"})

	queuedSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ravendb_cloud_request_queued_seconds",
		Help:    "How long a request waited for its turn before it was sent.",
		Buckets: []float64{0.01, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60},
	})
)

// Collectors returns the metrics above, for registering on the registry the
// operator's metrics endpoint serves.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{attempts, attemptSeconds, retries, queuedSeconds}
}

// noAnswer labels an attempt that ended without an HTTP status.
const noAnswer = "none"

func observeAttempt(method string, status int, took time.Duration) {
	code := noAnswer
	if status != 0 {
		code = strconv.Itoa(status)
	}
	attempts.WithLabelValues(method, code).Inc()
	attemptSeconds.WithLabelValues(method, code).Observe(took.Seconds())
}
