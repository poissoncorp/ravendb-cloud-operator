// Command mockcloud serves the RavenDB Cloud simulator in internal/cloudsim
// over HTTP, so the labs run end to end with no account, no key and no bill.
// It is only flags and a listener, which keeps the labs and the Go tests on
// the same simulator.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ravendb/ravendb-cloud-operator/internal/cloudsim"
)

func main() {
	var (
		addr              = flag.String("addr", ":8080", "address to listen on")
		apiKey            = flag.String("api-key", "lab-api-key", "the API key clients must present in X-Api-Key")
		domain            = flag.String("domain", "acme.ravendb.cloud", "the account DNS domain")
		provisionDuration = flag.Duration("provision-duration", 25*time.Second, "how long a create stays in Creating")
		changeDuration    = flag.Duration("change-duration", 12*time.Second, "how long a reshape stays in DeployingChanges")
		throttle          = flag.Bool("throttle", false, "answer 429 to a call that comes too soon after the previous one to the same route")
		seed              = flag.String("seed", "", "comma separated display names to pre-create as Active clusters")
		certPassword      = flag.String("cert-password", "lab-pfx-password", "password for the protected .pfx in the certificate bundle")
	)
	flag.Parse()

	sim := cloudsim.New(cloudsim.Options{
		Logf:              log.Printf,
		APIKey:            *apiKey,
		Domain:            *domain,
		ProvisionDuration: *provisionDuration,
		ChangeDuration:    *changeDuration,
		Throttle:          *throttle,
		CertPassword:      *certPassword,
	})

	for _, name := range splitList(*seed) {
		c := sim.State().Seed(cloudsim.SeedSpec{DisplayName: name})
		log.Printf("seeded pre-existing cluster %q with id %s", c.DisplayName, c.ID)
	}

	log.Printf("mock RavenDB Cloud listening on %s", *addr)
	log.Printf("  api key:    %s", *apiKey)
	log.Printf("  domain:     %s", *domain)
	log.Printf("  provision:  %s, change: %s, throttle: %v", *provisionDuration, *changeDuration, *throttle)

	if err := http.ListenAndServe(*addr, sim.Handler()); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

// splitList parses the -seed flag, skipping empty entries so a trailing comma
// is not a cluster named "".
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
