// Package cloudclients turns a CloudAccount into an authenticated
// cloudapi.Client, reading the API key from the Secret the account names.
//
// Clients are cached because the rate limiter lives inside them. Rebuilding one
// per reconcile would hand back a fresh limiter every time and undo the pacing.
package cloudclients

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/time/rate"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// Cache holds one cloud API client per CloudAccount, and one rate limiter for
// all of them.
//
// Share one Cache across every controller in the process, so every account
// draws on one pace rather than one each.
type Cache struct {
	// Kube reads the credential Secrets. Not the cloudapi.Client this hands out.
	Kube      client.Client
	UserAgent string

	// Endpoint is the control plane every account uses. Empty means
	// cloudapi.DefaultEndpoint. Only whoever runs the operator sets it, never a
	// CloudAccount: see CloudAccountSpec.
	Endpoint string

	// Pacing for every client built here. The zero value is production pacing.
	Pacing Pacing

	mu      sync.Mutex
	entries map[types.NamespacedName]entry
	limiter *rate.Limiter
}

// Pacing is how hard a client may push the API. The zero value is production:
// cloudapi.DefaultRateLimit, three retries, backoff in seconds.
type Pacing struct {
	RateLimit      rate.Limit
	RetryBaseDelay time.Duration

	// Negative means a single attempt, which is what makes an injected failure
	// observable in a test instead of being retried away.
	MaxRetries int
}

type entry struct {
	fingerprint string
	client      cloudapi.Client
}

// NewCache returns a Cache with production pacing.
func NewCache(kube client.Client, userAgent string) *Cache {
	return &Cache{Kube: kube, UserAgent: userAgent}
}

// CredentialMissingError means the Secret or key is not there yet. Controllers
// wait and watch rather than failing: it often lands moments after the account.
type CredentialMissingError struct {
	Reason string
}

func (e *CredentialMissingError) Error() string { return e.Reason }

// ClientFor returns the account's client, building one if the credential or
// endpoint has changed since last time.
func (c *Cache) ClientFor(ctx context.Context, account *v1alpha1.CloudAccount) (cloudapi.Client, error) {
	apiKey, err := c.apiKey(ctx, account)
	if err != nil {
		return nil, err
	}
	endpoint := cmp.Or(strings.TrimSpace(c.Endpoint), cloudapi.DefaultEndpoint)
	fingerprint := fingerprintOf(endpoint, apiKey)
	key := types.NamespacedName{Namespace: account.Namespace, Name: account.Name}

	c.mu.Lock()
	defer c.mu.Unlock()
	cached, existed := c.entries[key]
	if existed && cached.fingerprint == fingerprint {
		return cached.client, nil
	}
	if existed {
		// Logged although nothing failed: it is the first thing to check when
		// a rotated key did not apply. The fingerprint is a hash, safe to print.
		logr.FromContextOrDiscard(ctx).Info("credential or endpoint changed, rebuilding the cloud client",
			"account", key.String(), "endpoint", endpoint, "fingerprint", fingerprint[:12])
	}

	cloudClient, err := cloudapi.NewRESTClient(cloudapi.Options{
		Endpoint:       endpoint,
		APIKey:         apiKey,
		UserAgent:      c.UserAgent,
		Limiter:        c.sharedLimiter(),
		RetryBaseDelay: c.Pacing.RetryBaseDelay,
		MaxRetries:     c.Pacing.MaxRetries,
	})
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = map[types.NamespacedName]entry{}
	}
	c.entries[key] = entry{fingerprint: fingerprint, client: cloudClient}
	return cloudClient, nil
}

// LastFor returns the client last built for the account, whatever has happened
// to its credential Secret since. Deletion uses it to terminate a cluster whose
// Secret went in the same kubectl delete.
func (c *Cache) LastFor(account *v1alpha1.CloudAccount) (cloudapi.Client, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.entries[types.NamespacedName{Namespace: account.Namespace, Name: account.Name}]
	return cached.client, ok
}

// sharedLimiter is the one limiter for the whole process. Call it with mu held.
func (c *Cache) sharedLimiter() *rate.Limiter {
	if c.limiter == nil {
		c.limiter = rate.NewLimiter(cmp.Or(c.Pacing.RateLimit, cloudapi.DefaultRateLimit), 1)
	}
	return c.limiter
}

// Forget drops a deleted account's client, so its credential stops living in
// operator memory.
func (c *Cache) Forget(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, types.NamespacedName{Namespace: namespace, Name: name})
}

func (c *Cache) apiKey(ctx context.Context, account *v1alpha1.CloudAccount) (string, error) {
	secretName := account.Spec.APIKeySecretRef.Name
	if secretName == "" {
		return "", &CredentialMissingError{Reason: "spec.apiKeySecretRef.name is empty"}
	}
	keyName := account.EffectiveSecretKey()

	var secret corev1.Secret
	err := c.Kube.Get(ctx, types.NamespacedName{Namespace: account.Namespace, Name: secretName}, &secret)
	if apierrors.IsNotFound(err) {
		return "", &CredentialMissingError{
			Reason: fmt.Sprintf("Secret %q not found in namespace %q", secretName, account.Namespace),
		}
	}
	if err != nil {
		return "", err
	}
	raw, ok := secret.Data[keyName]
	if !ok {
		return "", &CredentialMissingError{
			Reason: fmt.Sprintf("Secret %q has no key %q", secretName, keyName),
		}
	}
	apiKey := strings.TrimSpace(string(raw))
	if apiKey == "" {
		return "", &CredentialMissingError{
			Reason: fmt.Sprintf("Secret %q key %q is empty", secretName, keyName),
		}
	}
	return apiKey, nil
}

// fingerprintOf identifies a credential and endpoint pair, so a rotated key no
// longer matches its cached client. It is a hash so that this bookkeeping value
// is not a second copy of the key.
func fingerprintOf(endpoint, apiKey string) string {
	h := sha256.New()
	h.Write([]byte(endpoint))
	// A zero byte between the two, so a different split of the same characters
	// cannot produce the same hash.
	h.Write([]byte{0})
	h.Write([]byte(apiKey))
	return hex.EncodeToString(h.Sum(nil))
}
