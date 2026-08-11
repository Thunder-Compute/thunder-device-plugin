package operator

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/thunderclient"
)

// reapOrphanedClients revokes and removes ThunderClients whose ResourceClaim is
// gone.
//
// The daemon holds each ThunderClient with a finalizer so that deleting it, or
// its namespace, cannot silently leak a Thunder enrollment. Normally the daemon
// releases that finalizer during unprepare. When it cannot -- the node was
// removed, or the resource was deleted out from under a live claim -- nothing
// else would ever release it, so the resource would wedge and the enrollment
// would keep consuming zone capacity. This is the backstop for that.
//
// It waits out a grace period before acting. A claim is deleted slightly before
// the kubelet finishes unpreparing it, and revoking an enrollment out from under
// a workload that is still shutting down would be worse than reaping late.
func (o *Operator) reapOrphanedClients(ctx context.Context) error {
	if o.clients == nil {
		return nil
	}

	list, err := o.clients.Resource(thunderclient.GVR).Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The CRD is not installed; nothing to reap.
			return nil
		}
		return fmt.Errorf("list ThunderClients: %w", err)
	}

	seen := make(map[string]struct{}, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		key := item.GetNamespace() + "/" + item.GetName()
		seen[key] = struct{}{}

		alive, err := o.claimIsAlive(ctx, item)
		if err != nil {
			return err
		}
		if alive {
			delete(o.orphans, key)
			continue
		}

		firstSeen, ok := o.orphans[key]
		if !ok {
			o.orphans[key] = o.now()
			o.logger.Info("ThunderClient has no ResourceClaim; waiting before reaping",
				"name", key, "grace", o.cfg.OrphanGracePeriod)
			continue
		}
		if o.now().Sub(firstSeen) < o.cfg.OrphanGracePeriod {
			continue
		}

		if err := o.reap(ctx, item); err != nil {
			return err
		}
		delete(o.orphans, key)
	}

	// Forget resources that no longer exist, so the map cannot grow forever.
	for key := range o.orphans {
		if _, ok := seen[key]; !ok {
			delete(o.orphans, key)
		}
	}
	return nil
}

// claimIsAlive reports whether the ResourceClaim a ThunderClient was created
// for still exists. A claim recreated under the same name is a different claim,
// so the UID has to match too.
func (o *Operator) claimIsAlive(ctx context.Context, item *unstructured.Unstructured) (bool, error) {
	namespace, _, _ := unstructured.NestedString(item.Object, "spec", "claimNamespace")
	name, _, _ := unstructured.NestedString(item.Object, "spec", "claimName")
	uid, _, _ := unstructured.NestedString(item.Object, "spec", "claimUID")

	if namespace == "" || name == "" {
		// Nothing to correlate against; leave it alone rather than guess.
		return true, nil
	}

	claim, err := o.kube.ResourceV1().ResourceClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get ResourceClaim %s/%s: %w", namespace, name, err)
	}
	if uid != "" && claim.UID != types.UID(uid) {
		return false, nil
	}
	return true, nil
}

// reap revokes the Thunder enrollment, then releases the finalizer so the
// resource can go away. Revoking first means a failure here leaves the resource
// in place to be retried, rather than dropping the only record of the leak.
func (o *Operator) reap(ctx context.Context, item *unstructured.Unstructured) error {
	name := item.GetNamespace() + "/" + item.GetName()
	tokenID, _, _ := unstructured.NestedString(item.Object, "status", "enrollmentTokenID")

	if tokenID != "" {
		if _, err := o.thunder.UnenrollClient(ctx, tokenID); err != nil && !isGone(err) {
			return fmt.Errorf("revoke enrollment %s for %s: %w", tokenID, name, err)
		}
	}

	resource := o.clients.Resource(thunderclient.GVR).Namespace(item.GetNamespace())
	if kept := withoutFinalizer(item.GetFinalizers()); len(kept) != len(item.GetFinalizers()) {
		updated := item.DeepCopy()
		updated.SetFinalizers(kept)
		if _, err := resource.Update(ctx, updated, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release finalizer on %s: %w", name, err)
		}
	}
	if err := resource.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s: %w", name, err)
	}

	o.logger.Info("reaped orphaned ThunderClient", "name", name, "enrollmentTokenID", tokenID)
	return nil
}

func withoutFinalizer(finalizers []string) []string {
	kept := make([]string, 0, len(finalizers))
	for _, finalizer := range finalizers {
		if finalizer != thunderclient.Finalizer {
			kept = append(kept, finalizer)
		}
	}
	return kept
}

// now is overridable so tests do not have to wait out the grace period.
func (o *Operator) now() time.Time {
	if o.clock != nil {
		return o.clock()
	}
	return time.Now()
}
