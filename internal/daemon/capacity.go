package daemon

import (
	"context"
	"errors"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/operator"
)

// OperatorCapacityRefresher lets the kubelet plugin synchronously reconcile
// the ResourceSlice pool selected by a new allocation.
type OperatorCapacityRefresher struct {
	Operator *operator.Operator
}

func (r OperatorCapacityRefresher) Refresh(ctx context.Context, allocation Allocation) error {
	if r.Operator == nil {
		return errors.New("capacity operator is required")
	}
	return r.Operator.SyncPool(ctx, allocation.Zone, allocation.GPUType)
}
