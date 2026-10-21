package controller

import (
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
)

// specFields maps a request field RavenDB Cloud names in a refusal to the spec
// field that sets it. Keys are lower case with any index or sub-field cut off.
// RavenDB Cloud names the property its validator checked, sometimes the
// request's own (instanceTypeName) and sometimes that of its internal copy of
// the request (storageSize), camel-cased.
var specFields = map[string]string{
	"displayname":      "spec.displayName",
	"subdomainname":    "spec.subdomain",
	"instancetype":     "spec.instanceType",
	"instancetypename": "spec.instanceType",
	"region":           "spec.region",
	"cloudprovider":    "spec.provider",
	"tier":             "spec.tier",
	"releasechannel":   "spec.releaseChannel",
	"disksize":         "spec.storage.sizeGB",
	"storagesize":      "spec.storage.sizeGB",
	"storagetype":      "spec.storage.type",
	"storagetypename":  "spec.storage.type",
	"iops":             "spec.storage.iops",
	"throughput":       "spec.storage.throughputMBps",
	"allowedips":       "spec.networking.allowedIPs",
}

// specField names the spec field behind a wire field, and leaves a field it
// does not know as RavenDB Cloud spelt it rather than guessing.
func specField(wire string) string {
	key := strings.ToLower(wire)
	if cut := strings.IndexAny(key, "[."); cut >= 0 {
		key = key[:cut]
	}
	if field, ok := specFields[key]; ok {
		return field
	}
	return wire
}

// refusedField reports whether RavenDB Cloud named this request field, spelt
// as the wire spells it, in what it refused. The field is structured data from
// the refusal body, not its wording.
func refusedField(err error, wire string) bool {
	var apiErr *cloudapi.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, p := range apiErr.Problems {
		if strings.EqualFold(p.Field, wire) {
			return true
		}
	}
	return false
}

// cloudSaid is what RavenDB Cloud said about a failed call, in words a user can
// act on, with any field it named turned into the spec field that sets it.
func cloudSaid(err error) string {
	return cloudapi.Describe(err, specField)
}

// sentence ends s with a full stop unless it already ends a sentence, so a
// message can quote RavenDB Cloud's words and go on without doubling it.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

// lowerFirst turns an event, which starts a sentence, into a phrase that can
// sit inside one: "Resizing storage" becomes "resizing storage".
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// saysAlready reports whether a condition already carries this reason and
// message, so a Warning is emitted when a problem starts rather than on every
// poll it lasts.
func saysAlready(conditions []metav1.Condition, condType, reason, message string) bool {
	cond := meta.FindStatusCondition(conditions, condType)
	return cond != nil && cond.Reason == reason && cond.Message == message
}
