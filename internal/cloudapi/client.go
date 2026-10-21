package cloudapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client is the whole contract between the operator and RavenDB Cloud.
//
// Every method honours context cancellation and is safe for concurrent use.
type Client interface {
	// --- Metadata --------------------------------------------------------

	// A successful call is the operator's proof that a credential is valid.
	GetAccountInfo(ctx context.Context) (*AccountInfo, error)

	ListRegions(ctx context.Context, provider Provider) ([]string, error)

	ListInstanceTypes(ctx context.Context, provider Provider, region string) ([]InstanceType, error)

	ListReleaseChannels(ctx context.Context) (*ReleaseChannels, error)

	// --- Lifecycle -------------------------------------------------------

	ListClusters(ctx context.Context) ([]ClusterRef, error)

	// ErrAmbiguous rather than a guess when the name is not unique, so
	// adoption never binds to an arbitrary cluster.
	FindClusterByName(ctx context.Context, displayName string) (*ClusterRef, error)

	GetCluster(ctx context.Context, id string) (*Cluster, error)

	// Returns as soon as the cluster is accepted. The API has no operation
	// handle, so the caller polls GetCluster until the status leaves Creating.
	CreateCluster(ctx context.Context, req CreateClusterRequest) (string, error)

	// Irreversible, and idempotent: an already gone (404) or already
	// terminating (410) cluster answers nil, or a CloudCluster whose cluster
	// was deleted in the portal could never release its finalizer.
	TerminateCluster(ctx context.Context, id string) error

	ChangeInstanceType(ctx context.Context, id, instanceType string) error

	ChangeStorage(ctx context.Context, id string, change StorageChange) error

	AddNode(ctx context.Context, id string) error

	RemoveNode(ctx context.Context, id, nodeTag string) error

	RestartNode(ctx context.Context, id, nodeTag string) error

	// --- Connectivity ----------------------------------------------------

	// A zip bundle, which the operator unpacks into a mountable Secret.
	GetClientCertificate(ctx context.Context, id string) (*ClientCertificate, error)
}

// Sentinel errors. Test with errors.Is or the Is* helpers, never by matching
// strings or status codes.
var (
	// ErrNotFound means RavenDB Cloud has no such cluster.
	ErrNotFound = errors.New("cloudapi: not found")

	// ErrAmbiguous means a lookup by name matched more than one cluster.
	ErrAmbiguous = errors.New("cloudapi: multiple clusters match")

	// ErrUnauthorized means the API key was rejected. Retrying will not help
	// until the user fixes the credential Secret.
	ErrUnauthorized = errors.New("cloudapi: unauthorized")

	// ErrTooSoon means RavenDB Cloud refused a change that it will accept later
	// without anybody editing anything.
	ErrTooSoon = errors.New("cloudapi: RavenDB Cloud will accept this later")

	// ErrBusy means RavenDB Cloud refused a change because the cluster is not
	// Active: another change or a restart is under way, and it will take this
	// one once that is done.
	ErrBusy = errors.New("cloudapi: the cluster is busy with another change")
)

// APIError carries an unexpected HTTP response from RavenDB Cloud.
//
// Error is for the log: it names the request and carries the body as sent.
// Describe is for the person reading kubectl describe, and says only what
// RavenDB Cloud said.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Body       string

	// Problems is what RavenDB Cloud said was wrong, read out of Body. Empty
	// when the body said nothing, which is what a 429, a 404 or a proxy's 502
	// usually carry.
	Problems []Problem
}

// Problem is one thing RavenDB Cloud said was wrong with a request, in its own
// words.
type Problem struct {
	// Field is the request field the problem is about, spelt as RavenDB Cloud
	// spells it, or empty when it is about the request as a whole.
	Field   string
	Message string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("cloudapi: %s %s: %s", e.Method, e.Path, e.Status())
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Status is the HTTP status as a person reads it, "401 Unauthorized".
func (e *APIError) Status() string { return statusText(e.StatusCode) }

func statusText(code int) string {
	status := strconv.Itoa(code)
	if text := http.StatusText(code); text != "" {
		status += " " + text
	}
	return status
}

// Unwrap maps the well-known status codes onto sentinels, so that
// errors.Is(err, ErrNotFound) works on a raw transport failure.
func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}
	return nil
}

// statusOf is the HTTP status behind err, or 0 when no answer arrived.
func statusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// AmbiguousNameError is a lookup by display name that matched more than one
// live cluster. errors.Is(err, ErrAmbiguous) holds for it.
type AmbiguousNameError struct {
	Name string
	IDs  []string
}

func (e *AmbiguousNameError) Error() string {
	return fmt.Sprintf("cloudapi: %d clusters named %q (%s)", len(e.IDs), e.Name, strings.Join(e.IDs, ", "))
}

func (e *AmbiguousNameError) Is(target error) bool { return target == ErrAmbiguous }

// DecodeError is an answer that arrived and did not have the shape this client
// reads. The same answer will not decode next time either, so it is not
// retryable: what fixes it is a newer operator, or RavenDB Cloud changing back.
type DecodeError struct {
	Path string
	Err  error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("cloudapi: decoding response from %s: %v", e.Path, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// Describe says what went wrong, for the person reading kubectl describe:
// RavenDB Cloud's own words when it sent any, the HTTP status when it did not,
// and why nothing came back when nothing did.
//
// fieldName renames a problem's field for the reader, which is how the
// controller turns a wire name into the spec field to change. Nil keeps
// RavenDB Cloud's spelling.
func Describe(err error, fieldName func(wire string) string) string {
	var (
		apiErr      *APIError
		ambiguous   *AmbiguousNameError
		undecodable *DecodeError
		unreachable *url.Error
		notSent     *notSentError
	)
	switch {
	case errors.As(err, &apiErr):
		return apiErr.describe(fieldName)
	case errors.As(err, &ambiguous):
		return fmt.Sprintf("%d RavenDB Cloud clusters are named %q (%s)",
			len(ambiguous.IDs), ambiguous.Name, strings.Join(ambiguous.IDs, ", "))
	case errors.As(err, &undecodable):
		return "RavenDB Cloud sent a response this version of the operator cannot read"
	case errors.As(err, &unreachable):
		return "no answer from RavenDB Cloud: " + unreachable.Err.Error()
	case errors.As(err, &notSent):
		return "ran out of time before asking RavenDB Cloud"
	}
	return err.Error()
}

func (e *APIError) describe(fieldName func(string) string) string {
	if len(e.Problems) == 0 {
		return e.Status()
	}
	if fieldName == nil {
		fieldName = func(wire string) string { return wire }
	}
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		if p.Field == "" {
			parts = append(parts, p.Message)
		} else {
			parts = append(parts, fieldName(p.Field)+": "+p.Message)
		}
	}
	said := strings.Join(parts, " ")
	// For a server failure, that it failed matters more than what it said.
	if e.StatusCode >= http.StatusInternalServerError {
		return e.Status() + ": " + said
	}
	return said
}

// IsNotFound reports whether err means the cluster asked for does not exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsAmbiguous reports whether err means a display name matched more than one
// cluster.
func IsAmbiguous(err error) bool { return errors.Is(err, ErrAmbiguous) }

// IsUnauthorized means a human has to fix the credential Secret, so callers
// report it on the CloudAccount rather than retrying.
func IsUnauthorized(err error) bool { return errors.Is(err, ErrUnauthorized) }

// IsTooSoon means the refusal lifts on its own, so the caller waits and asks
// again rather than treating it as final.
func IsTooSoon(err error) bool { return errors.Is(err, ErrTooSoon) }

// IsBusy means the cluster is applying another change or restarting, so the
// caller waits for it to be Active and asks again rather than giving up.
func IsBusy(err error) bool { return errors.Is(err, ErrBusy) }

// IsQuotaReached means the account holds as many products as its quota allows,
// which RavenDB Cloud answers with a 412. It lifts when a cluster is terminated
// elsewhere, without anything this caller can see changing.
func IsQuotaReached(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusPreconditionFailed
}

// IsRetryable reports whether another attempt could plausibly succeed.
// Rate limiting and server-side faults are retryable; a rejected key or a
// malformed request is not.
func IsRetryable(err error) bool {
	if err == nil || IsUnauthorized(err) || IsNotFound(err) || IsAmbiguous(err) {
		return false
	}
	var undecodable *DecodeError
	if errors.As(err, &undecodable) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	return true // dial, timeout, EOF
}

// MayHaveApplied reports whether a failed change could have taken effect
// anyway. A 5xx, a connection lost after the request went out, or a success
// whose body would not decode says nothing about whether RavenDB Cloud acted,
// and it takes no idempotency key, so the caller must look before sending the
// change again. A rate limit, a connection that never opened, and every other
// refusal are a definite no.
func MayHaveApplied(err error) bool {
	var undecodable *DecodeError
	if errors.As(err, &undecodable) {
		return true
	}
	return IsRetryable(err) && statusOf(err) != http.StatusTooManyRequests && !neverSent(err)
}

// neverSent reports a failure before the request left this process: the
// call was given up while it waited, or the connection was never opened.
func neverSent(err error) bool {
	var notSent *notSentError
	if errors.As(err, &notSent) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// notSentError is a call given up before it reached the network, waiting for
// its turn or for its next attempt, usually because the pass ran out of time.
type notSentError struct {
	Method, Path string
	Err          error
}

func (e *notSentError) Error() string {
	return fmt.Sprintf("cloudapi: %s %s was given up before it was sent: %v", e.Method, e.Path, e.Err)
}

func (e *notSentError) Unwrap() error { return e.Err }
