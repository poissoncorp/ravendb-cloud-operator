package controller

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ravendb/ravendb-cloud-operator/api/v1alpha1"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudapi"
	"github.com/ravendb/ravendb-cloud-operator/internal/cloudclients"
)

// accountWait is why a CloudAccount cannot be used yet, in words for the
// object that names it.
type accountWait struct {
	// reason is WaitingForAccount, or the account's own reason when that says
	// more: KeyRejected or CloudUnreachable.
	reason  string
	message string

	absent bool // the CloudAccount does not exist
}

// usableAccount reads the CloudAccount ref names and the client its key builds.
// A non-nil wait means it cannot be used yet, and says why.
func usableAccount(
	ctx context.Context, c client.Client, clients *cloudclients.Cache, ref types.NamespacedName,
) (*v1alpha1.CloudAccount, cloudapi.Client, *accountWait, error) {
	var account v1alpha1.CloudAccount
	if err := c.Get(ctx, ref, &account); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, &accountWait{
				reason:  v1alpha1.ReasonWaitingForAccount,
				message: fmt.Sprintf("CloudAccount %q not found in namespace %q.", ref.Name, ref.Namespace),
				absent:  true,
			}, nil
		}
		return nil, nil, nil, err
	}

	if !meta.IsStatusConditionTrue(account.Status.Conditions, v1alpha1.ConditionReady) {
		return &account, nil, notReady(&account), nil
	}

	cloud, err := clients.ClientFor(ctx, &account)
	var missing *cloudclients.CredentialMissingError
	switch {
	case errors.As(err, &missing):
		return &account, nil, &accountWait{
			reason:  v1alpha1.ReasonWaitingForAccount,
			message: sentence(missing.Error()),
		}, nil
	case err != nil:
		return nil, nil, nil, err
	}
	return &account, cloud, nil, nil
}

// notReady says why the account is not Ready, in the account's own words. A
// rejected key and an unreachable RavenDB Cloud keep their reasons, so the
// object and its account say the same word.
func notReady(account *v1alpha1.CloudAccount) *accountWait {
	wait := &accountWait{
		reason:  v1alpha1.ReasonWaitingForAccount,
		message: fmt.Sprintf("CloudAccount %q is not usable yet.", account.Name),
	}
	cond := meta.FindStatusCondition(account.Status.Conditions, v1alpha1.ConditionReady)
	if cond == nil {
		return wait
	}
	if cond.Message != "" {
		wait.message = fmt.Sprintf("CloudAccount %q: %s", account.Name, sentence(cond.Message))
	}
	if cond.Reason == v1alpha1.ReasonKeyRejected || cond.Reason == v1alpha1.ReasonCloudUnreachable {
		wait.reason = cond.Reason
	}
	return wait
}
