// Package thunderclient holds the identity of the per-claim ThunderClient
// resource, shared by the node daemon that writes it and the operator that
// garbage collects it. Keeping it in one place stops the two from drifting on
// a name that has to match exactly.
package thunderclient

import "k8s.io/apimachinery/pkg/runtime/schema"

// GVR addresses the ThunderClient custom resource.
var GVR = schema.GroupVersionResource{
	Group:    "thundercompute.com",
	Version:  "v1alpha1",
	Resource: "clients",
}

// Finalizer keeps a ThunderClient alive until its Thunder enrollment has been
// revoked. Without it, deleting the resource, or the namespace holding it,
// silently leaks a client enrollment that keeps consuming zone capacity: the
// daemon's unprepare treats a missing resource as nothing to do.
//
// It is removed by the daemon on unprepare, and by the operator for resources
// whose ResourceClaim is gone.
const Finalizer = "thundercompute.com/client-cleanup"
