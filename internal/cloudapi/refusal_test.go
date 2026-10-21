package cloudapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// refusing answers every request with one status and body.
func refusing(t *testing.T, status int, body string) Client {
	t.Helper()
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return client
}

// TestARefusalCarriesRavenDBCloudsOwnWords reads both shapes a refusal comes
// in. Without it the whole body would reach kubectl describe as raw JSON,
// behind a Go package name, an HTTP method and an API path.
func TestARefusalCarriesRavenDBCloudsOwnWords(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Problem
	}{
		{
			name: "a validator's verdict, the shape measured against the real service",
			body: `{"subdomainName":["Domain name is too long."]}`,
			want: []Problem{{Field: "subdomainName", Message: "Domain name is too long."}},
		},
		{
			name: "a problem with the whole request comes first, fields after it in order",
			body: `{"tier":["Tier is not available."],"generalError":["The product is not in the Active status."],"region":["Unknown region."]}`,
			want: []Problem{
				{Message: "The product is not in the Active status."},
				{Field: "region", Message: "Unknown region."},
				{Field: "tier", Message: "Tier is not available."},
			},
		},
		{
			name: "a body the framework refused before any validator ran",
			body: `{"type":"https://tools.ietf.org/html/rfc9110#section-15.5.1","title":"One or more validation errors occurred.","status":400,"errors":{"ProductId":["The ProductId field is required."]}}`,
			want: []Problem{{Field: "ProductId", Message: "The ProductId field is required."}},
		},
		{name: "no body at all", body: ``},
		{name: "a proxy's error page", body: `<html><body>502 Bad Gateway</body></html>`},
		{name: "a title and nothing else says nothing the status does not", body: `{"title":"Not Found","status":404}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := refusing(t, http.StatusBadRequest, tc.body).GetAccountInfo(context.Background())
			apiErr, ok := err.(*APIError)
			if !ok {
				t.Fatalf("err = %T %v, want *APIError", err, err)
			}
			if len(apiErr.Problems) != len(tc.want) {
				t.Fatalf("problems = %+v, want %+v", apiErr.Problems, tc.want)
			}
			for i := range tc.want {
				if apiErr.Problems[i] != tc.want[i] {
					t.Errorf("problem %d = %+v, want %+v", i, apiErr.Problems[i], tc.want[i])
				}
			}
		})
	}
}

// TestDescribeLeavesTheWireOut is the other half: what reaches a person is what
// RavenDB Cloud said, and the request line stays in Error, for the log.
func TestDescribeLeavesTheWireOut(t *testing.T) {
	_, err := refusing(t, http.StatusBadRequest, `{"subdomainName":["Domain name is too long."]}`).
		CreateCluster(context.Background(), validCreateRequest())

	described := Describe(err, nil)
	if described != "subdomainName: Domain name is too long." {
		t.Errorf("Describe = %q", described)
	}
	for _, wire := range []string{"cloudapi", "POST", "/api/v1", "{", "400"} {
		if strings.Contains(described, wire) {
			t.Errorf("Describe = %q carries %q, which a user cannot act on", described, wire)
		}
	}
	// The log still gets all of it.
	if logged := err.Error(); !strings.Contains(logged, "POST /api/v1/products/create") || !strings.Contains(logged, "400") {
		t.Errorf("Error = %q lost the request line the log needs", logged)
	}

	renamed := Describe(err, func(wire string) string { return "spec." + wire })
	if renamed != "spec.subdomainName: Domain name is too long." {
		t.Errorf("Describe with a field namer = %q", renamed)
	}
}

// TestDescribeSaysWhatHappenedWhenRavenDBCloudSaidNothing covers the answers
// that carry no words of their own.
func TestDescribeSaysWhatHappenedWhenRavenDBCloudSaidNothing(t *testing.T) {
	_, err := refusing(t, http.StatusServiceUnavailable, ``).GetAccountInfo(context.Background())
	if got := Describe(err, nil); got != "503 Service Unavailable" {
		t.Errorf("an empty 503 described as %q", got)
	}

	client, err := NewRESTClient(Options{Endpoint: "http://127.0.0.1:1", APIKey: "k", MaxRetries: -1})
	if err != nil {
		t.Fatalf("NewRESTClient: %v", err)
	}
	_, err = client.GetAccountInfo(context.Background())
	if got := Describe(err, nil); !strings.HasPrefix(got, "no answer from RavenDB Cloud: ") || strings.Contains(got, "/api/v1") {
		t.Errorf("an unreachable endpoint described as %q", got)
	}
}

// TestATooSoonStorageChangeIsNotFinal is the refusal lab 05 met against the
// real service: the second of two storage changes a few minutes apart. It
// lifts on its own, so it must not look like one that never will.
func TestATooSoonStorageChangeIsNotFinal(t *testing.T) {
	const tooSoon = `{"generalError":["You've reached the maximum storage modification rate. Wait at least 6 hours between each storage change. The next storage change will be available in 5 hours 58 minutes."]}`

	err := refusing(t, http.StatusBadRequest, tooSoon).
		ChangeStorage(context.Background(), "1-A", StorageChange{SizeGiB: 30, Type: StorageSsdStandard})
	if !IsTooSoon(err) {
		t.Fatalf("IsTooSoon(%v) = false", err)
	}
	if IsRetryable(err) {
		t.Error("IsRetryable = true: a backoff in milliseconds is not what waiting six hours looks like")
	}
	if got := Describe(err, nil); !strings.Contains(got, "available in 5 hours 58 minutes") {
		t.Errorf("Describe = %q lost when it will be allowed", got)
	}

	other := refusing(t, http.StatusBadRequest, `{"storageSize":["The selected storage size is not allowed."]}`).
		ChangeStorage(context.Background(), "1-A", StorageChange{SizeGiB: 31, Type: StorageSsdStandard})
	if IsTooSoon(other) {
		t.Error("an off-menu size is final, and was classified as one that lifts on its own")
	}
}

// TestAnAnswerThatWillNotDecodeIsNotRetried: the same body decodes the same way
// on every attempt, so retrying it only makes more calls.
func TestAnAnswerThatWillNotDecodeIsNotRetried(t *testing.T) {
	_, err := refusing(t, http.StatusOK, `{"domainName": 42}`).GetAccountInfo(context.Background())
	if err == nil {
		t.Fatal("a number where a string belongs decoded without complaint")
	}
	if IsRetryable(err) {
		t.Errorf("IsRetryable(%v) = true", err)
	}
	if got := Describe(err, nil); !strings.Contains(got, "cannot read") {
		t.Errorf("Describe = %q", got)
	}
}

func validCreateRequest() CreateClusterRequest {
	return CreateClusterRequest{
		DisplayName: "orders", SubdomainName: "orders", Provider: ProviderAWS, Region: "eu-central-1",
		Tier: TierDevelopment, InstanceType: "Dev10", ReleaseChannel: "Stable62",
		Storage:    Storage{SizeGiB: 10, Type: StorageSsdStandard},
		AllowedIPs: []string{"192.0.2.1/32"},
	}
}
