package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	thunder "github.com/Thunder-Compute/thunder-sdk"
)

// thunderDomain qualifies every resource, attribute and label the driver owns.
const thunderDomain = "thundercompute.com"

const (
	DefaultDriverName             = thunderDomain
	DefaultThunderClientNamespace = "thunder-system"
	DefaultCDIKind                = thunderDomain + "/gpu"

	GPUTypeAttributeName = thunderDomain + "/gpu_type"
	ZoneAttributeName    = thunderDomain + "/zone"

	claimUIDLabelName       = thunderDomain + "/claim-uid"
	claimNameLabelName      = thunderDomain + "/claim-name"
	claimNamespaceLabelName = thunderDomain + "/claim-namespace"
	gpuTypeLabelName        = thunderDomain + "/gpu-type"
)

var ErrNotFound = errors.New("not found")

type Allocation struct {
	ClaimUID       types.UID
	ClaimNamespace string
	ClaimName      string

	// Devices holds every device the claim was allocated from this driver.
	// The operator publishes one device per GPU, so a multi-GPU claim has one
	// entry per GPU and GPUCount is simply how many there are.
	Devices []AllocatedDevice

	RequestName string
	PoolName    string
	DeviceName  string
	ShareID     *types.UID

	Consumer ResourceConsumer
	NodeName string
	Zone     string
	GPUType  string
	GPUCount int64
}

// AllocatedDevice is one GPU the scheduler assigned to a claim.
type AllocatedDevice struct {
	RequestName string
	PoolName    string
	DeviceName  string
	ShareID     *types.UID
}

type ResourceConsumer struct {
	APIGroup  string
	Resource  string
	Namespace string
	Name      string
	UID       types.UID
}

type ThunderClient struct {
	ClaimUID       types.UID
	ClaimNamespace string
	ClaimName      string

	GPUType  string
	GPUCount int64
	Zone     string
	NodeName string

	RequestName string
	PoolName    string
	DeviceName  string
	ShareID     *types.UID
	Consumer    ResourceConsumer

	CDIName           string
	EnrollmentTokenID string
	GuestNamespace    string
	GuestSecret       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type TokenIssuer interface {
	Mint(ctx context.Context, allocation Allocation) (tokenID string, token string, expiresAt time.Time, err error)
	Revoke(ctx context.Context, tokenID string) error
	// RevokeClient revokes the client an enrollment token was exchanged for.
	// Revoking the token alone does not: once spent, the token and the client
	// it produced are separate objects in Thunder.
	RevokeClient(ctx context.Context, clientID string) error
}

type ThunderClientStore interface {
	Get(ctx context.Context, claimUID types.UID) (*ThunderClient, error)
	Upsert(ctx context.Context, client ThunderClient) error
	Delete(ctx context.Context, claimUID types.UID) error
}

type CDIDeviceStore interface {
	Create(ctx context.Context, allocation Allocation, token string) (qualifiedName string, err error)
	Remove(ctx context.Context, qualifiedName string) error
	// StagedClientID reports the Thunder client the CDI hook enrolled for this
	// device, or "" if no container ever started and none was created.
	StagedClientID(qualifiedName string) string
}

type GuestArtifacts struct {
	Namespace  string
	SecretName string
}

type GuestConfigStore interface {
	Create(ctx context.Context, allocation Allocation, token string, installCommand string) (GuestArtifacts, error)
	Remove(ctx context.Context, artifacts GuestArtifacts) error
}

type Driver struct {
	DriverName string
	NodeName   string
	Kube       kubernetes.Interface
	Tokens     TokenIssuer
	Clients    ThunderClientStore
	CDI        CDIDeviceStore
	Guest      GuestConfigStore
	Logger     *slog.Logger
}

var _ kubeletplugin.DRAPlugin = (*Driver)(nil)

func (d *Driver) PrepareResourceClaims(ctx context.Context, claims []*resourcev1.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		devices, err := d.prepareOne(ctx, claim)
		result[claim.UID] = kubeletplugin.PrepareResult{Err: err, Devices: devices}
	}
	return result, nil
}

func (d *Driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		result[claim.UID] = d.unprepareOne(ctx, claim)
	}
	return result, nil
}

func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
	runtime.HandleErrorWithContext(ctx, err, msg)
}

func (d *Driver) prepareOne(ctx context.Context, claim *resourcev1.ResourceClaim) ([]kubeletplugin.Device, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}

	allocation, err := d.decodeAllocation(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("decode allocation: %w", err)
	}

	existing, err := d.Clients.Get(ctx, claim.UID)
	switch {
	case err == nil && existing.EnrollmentTokenID != "" && existing.CDIName != "":
		return devicesFor(existing.CDIName, allocation), nil
	case err != nil && !errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("load ThunderClient: %w", err)
	}

	tokenID, token, _, err := d.Tokens.Mint(ctx, allocation)
	if err != nil {
		return nil, fmt.Errorf("mint client enrollment token: %w", err)
	}

	guestArtifacts, err := d.Guest.Create(ctx, allocation, token, d.CDIInstallCommand())
	if err != nil {
		_ = d.Tokens.Revoke(ctx, tokenID)
		return nil, fmt.Errorf("create guest Thunder artifacts: %w", err)
	}

	cdiName, err := d.CDI.Create(ctx, allocation, token)
	if err != nil {
		_ = d.Guest.Remove(ctx, guestArtifacts)
		_ = d.Tokens.Revoke(ctx, tokenID)
		return nil, fmt.Errorf("create CDI device: %w", err)
	}

	client := thunderClientFromAllocation(allocation)
	client.CDIName = cdiName
	client.EnrollmentTokenID = tokenID
	client.GuestNamespace = guestArtifacts.Namespace
	client.GuestSecret = guestArtifacts.SecretName
	if err := d.Clients.Upsert(ctx, client); err != nil {
		_ = d.CDI.Remove(ctx, cdiName)
		_ = d.Guest.Remove(ctx, guestArtifacts)
		_ = d.Tokens.Revoke(ctx, tokenID)
		return nil, fmt.Errorf("upsert ThunderClient: %w", err)
	}

	if d.Logger != nil {
		d.Logger.Info("prepared Thunder ResourceClaim", "claim", claim.Namespace+"/"+claim.Name, "claimUID", claim.UID, "cdi", cdiName, "tokenID", tokenID)
	}
	return devicesFor(cdiName, allocation), nil
}

func (d *Driver) unprepareOne(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	if err := d.validateStoresOnly(); err != nil {
		return err
	}

	client, err := d.Clients.Get(ctx, claim.UID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("load ThunderClient: %w", err)
	}

	// The client has to go before the CDI state that records its ID does.
	// A claim whose container never started has no client, only an unspent
	// token, and revoking the token below is the whole cleanup.
	clientID := ""
	if strings.TrimSpace(client.CDIName) != "" {
		clientID = d.CDI.StagedClientID(client.CDIName)
	}
	if clientID != "" {
		if err := d.Tokens.RevokeClient(ctx, clientID); err != nil {
			return fmt.Errorf("revoke thunder client %q: %w", clientID, err)
		}
	}

	if strings.TrimSpace(client.EnrollmentTokenID) != "" {
		if err := d.Tokens.Revoke(ctx, client.EnrollmentTokenID); err != nil {
			return fmt.Errorf("revoke enrollment token %q: %w", client.EnrollmentTokenID, err)
		}
	}

	if strings.TrimSpace(client.CDIName) != "" {
		if err := d.CDI.Remove(ctx, client.CDIName); err != nil {
			return fmt.Errorf("remove CDI device %q: %w", client.CDIName, err)
		}
	}

	if err := d.Guest.Remove(ctx, GuestArtifacts{
		Namespace:  firstNonEmpty(client.GuestNamespace, client.ClaimNamespace, claim.Namespace),
		SecretName: client.GuestSecret,
	}); err != nil {
		return fmt.Errorf("remove guest Thunder artifacts: %w", err)
	}

	if err := d.Clients.Delete(ctx, claim.UID); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("delete ThunderClient: %w", err)
	}
	if d.Logger != nil {
		d.Logger.Info("unprepared Thunder ResourceClaim", "claim", claim.String(),
			"tokenID", client.EnrollmentTokenID, "clientID", clientID)
	}
	return nil
}

func (d *Driver) decodeAllocation(ctx context.Context, claim *resourcev1.ResourceClaim) (Allocation, error) {
	if claim.Status.Allocation == nil {
		return Allocation{}, errors.New("claim is not allocated")
	}

	driverName := d.driverName()
	allocation := Allocation{
		ClaimUID:       claim.UID,
		ClaimNamespace: claim.Namespace,
		ClaimName:      claim.Name,
		Consumer:       firstConsumer(claim),
		NodeName:       d.NodeName,
	}

	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != driverName {
			continue
		}

		device, err := d.allocatedDevice(ctx, result.Pool, result.Device)
		if err != nil {
			return Allocation{}, err
		}
		zone := stringAttribute(device, ZoneAttributeName)
		gpuType := stringAttribute(device, GPUTypeAttributeName)
		if zone == "" || gpuType == "" {
			return Allocation{}, fmt.Errorf("allocated device %s/%s is missing Thunder zone or gpu type attributes", result.Pool, result.Device)
		}
		// Every device in one claim comes from a single zone and GPU type: the
		// pool is keyed on both, and a request resolves within one pool.
		if allocation.Zone != "" && (allocation.Zone != zone || allocation.GPUType != gpuType) {
			return Allocation{}, fmt.Errorf("claim mixes Thunder pools: %s/%s and %s/%s",
				allocation.Zone, allocation.GPUType, zone, gpuType)
		}
		allocation.Zone = zone
		allocation.GPUType = gpuType

		allocation.Devices = append(allocation.Devices, AllocatedDevice{
			RequestName: result.Request,
			PoolName:    result.Pool,
			DeviceName:  result.Device,
			ShareID:     result.ShareID,
		})
	}

	if len(allocation.Devices) == 0 {
		return Allocation{}, fmt.Errorf("claim has no allocation result for driver %s", driverName)
	}

	// One GPU per allocated device.
	allocation.GPUCount = int64(len(allocation.Devices))

	// The first device names the claim in the ThunderClient status and in
	// logs; the CDI device itself is per claim, not per GPU.
	first := allocation.Devices[0]
	allocation.RequestName = first.RequestName
	allocation.PoolName = first.PoolName
	allocation.DeviceName = first.DeviceName
	allocation.ShareID = first.ShareID
	return allocation, nil
}

func (d *Driver) allocatedDevice(ctx context.Context, poolName, deviceName string) (*resourcev1.Device, error) {
	if d.Kube == nil {
		return nil, errors.New("kubernetes client is required")
	}
	selector := fields.OneTermEqualSelector(resourcev1.ResourceSliceSelectorDriver, d.driverName()).String()
	list, err := d.Kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list ResourceSlices: %w", err)
	}
	for i := range list.Items {
		slice := &list.Items[i]
		if slice.Spec.Pool.Name != poolName {
			continue
		}
		for j := range slice.Spec.Devices {
			if slice.Spec.Devices[j].Name == deviceName {
				return &slice.Spec.Devices[j], nil
			}
		}
	}
	return nil, fmt.Errorf("allocated device %s/%s was not found in ResourceSlices", poolName, deviceName)
}

func (d *Driver) validate() error {
	if d.Kube == nil {
		return errors.New("kubernetes client is required")
	}
	return d.validateStoresOnly()
}

func (d *Driver) validateStoresOnly() error {
	if d.Tokens == nil {
		return errors.New("token issuer is required")
	}
	if d.Clients == nil {
		return errors.New("ThunderClient store is required")
	}
	if d.CDI == nil {
		return errors.New("CDI device store is required")
	}
	if d.Guest == nil {
		return errors.New("guest config store is required")
	}
	return nil
}

func (d *Driver) CDIInstallCommand() string {
	if store, ok := d.CDI.(*FileCDIDeviceStore); ok {
		return store.ClientInstallCommand
	}
	return ""
}

func (d *Driver) driverName() string {
	if strings.TrimSpace(d.DriverName) == "" {
		return DefaultDriverName
	}
	return strings.TrimSpace(d.DriverName)
}

// devicesFor reports one entry per allocated GPU. They all reference the same
// CDI device because a claim maps to a single Thunder client, however many GPUs
// that client was enrolled with.
func devicesFor(cdiName string, allocation Allocation) []kubeletplugin.Device {
	devices := make([]kubeletplugin.Device, 0, len(allocation.Devices))
	for _, device := range allocation.Devices {
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{device.RequestName},
			PoolName:     device.PoolName,
			DeviceName:   device.DeviceName,
			ShareID:      device.ShareID,
			CDIDeviceIDs: []string{cdiName},
		})
	}
	return devices
}

func firstConsumer(claim *resourcev1.ResourceClaim) ResourceConsumer {
	if len(claim.Status.ReservedFor) == 0 {
		return ResourceConsumer{}
	}
	consumer := claim.Status.ReservedFor[0]
	return ResourceConsumer{
		APIGroup:  consumer.APIGroup,
		Resource:  consumer.Resource,
		Namespace: claim.Namespace,
		Name:      consumer.Name,
		UID:       consumer.UID,
	}
}

func thunderClientFromAllocation(allocation Allocation) ThunderClient {
	now := time.Now().UTC()
	return ThunderClient{
		ClaimUID:       allocation.ClaimUID,
		ClaimNamespace: allocation.ClaimNamespace,
		ClaimName:      allocation.ClaimName,
		GPUType:        allocation.GPUType,
		GPUCount:       allocation.GPUCount,
		Zone:           allocation.Zone,
		NodeName:       allocation.NodeName,
		RequestName:    allocation.RequestName,
		PoolName:       allocation.PoolName,
		DeviceName:     allocation.DeviceName,
		ShareID:        allocation.ShareID,
		Consumer:       allocation.Consumer,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func stringAttribute(device *resourcev1.Device, name string) string {
	if device == nil || device.Attributes == nil {
		return ""
	}
	attribute, ok := device.Attributes[resourcev1.QualifiedName(name)]
	if !ok || attribute.StringValue == nil {
		return ""
	}
	return strings.TrimSpace(*attribute.StringValue)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func Quantity(value int64) apiresource.Quantity {
	return *apiresource.NewQuantity(value, apiresource.DecimalSI)
}

func IsNotFoundError(err error) bool {
	return errors.Is(err, ErrNotFound) || apierrors.IsNotFound(err)
}

// ThunderTokenIssuer adapts the Thunder SDK to the daemon DRA plugin.
type ThunderTokenIssuer struct {
	Client *thunder.Client
	ZoneID string
}

func (i ThunderTokenIssuer) Mint(ctx context.Context, allocation Allocation) (string, string, time.Time, error) {
	if i.Client == nil {
		return "", "", time.Time{}, errors.New("thunder client is required")
	}
	zoneID := strings.TrimSpace(i.ZoneID)
	if zoneID == "" {
		zoneID = allocation.Zone
	}
	token, err := i.Client.CreateClientEnrollment(ctx, thunder.CreateClientEnrollmentRequest{
		ZoneID:   zoneID,
		GPUType:  allocation.GPUType,
		GPUCount: uint64(allocation.GPUCount),
	})
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt := time.Time{}
	if token.ExpiresAt != nil {
		expiresAt = *token.ExpiresAt
	}
	return token.EnrollmentTokenID, token.EnrollmentToken, expiresAt, nil
}

// RevokeClient revokes a client that was enrolled by exchanging a token.
func (i ThunderTokenIssuer) RevokeClient(ctx context.Context, clientID string) error {
	if strings.TrimSpace(clientID) == "" {
		return nil
	}
	if i.Client == nil {
		return errors.New("thunder client is required")
	}
	_, err := i.Client.RevokeClient(ctx, clientID)
	if thunder.IsNotFound(err) {
		return nil
	}
	return err
}

func (i ThunderTokenIssuer) Revoke(ctx context.Context, tokenID string) error {
	if strings.TrimSpace(tokenID) == "" {
		return nil
	}
	if i.Client == nil {
		return errors.New("thunder client is required")
	}
	_, err := i.Client.UnenrollClient(ctx, tokenID)
	if thunder.IsNotFound(err) {
		return nil
	}
	return err
}
