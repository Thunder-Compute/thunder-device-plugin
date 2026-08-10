package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/thunderclient"
)

// ThunderClientGVR addresses the per-claim ThunderClient resource.
var ThunderClientGVR = thunderclient.GVR

type KubernetesThunderClientStore struct {
	Client    dynamic.Interface
	Namespace string
}

func NewKubernetesThunderClientStore(client dynamic.Interface, namespace string) *KubernetesThunderClientStore {
	if strings.TrimSpace(namespace) == "" {
		namespace = DefaultThunderClientNamespace
	}
	return &KubernetesThunderClientStore{Client: client, Namespace: namespace}
}

func (s *KubernetesThunderClientStore) Get(ctx context.Context, claimUID types.UID) (*ThunderClient, error) {
	if s.Client == nil {
		return nil, fmt.Errorf("dynamic client is required")
	}
	obj, err := s.Client.Resource(ThunderClientGVR).Namespace(s.namespace()).Get(ctx, ThunderClientName(claimUID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return thunderClientFromObject(obj), nil
}

func (s *KubernetesThunderClientStore) Upsert(ctx context.Context, client ThunderClient) error {
	if s.Client == nil {
		return fmt.Errorf("dynamic client is required")
	}
	if client.CreatedAt.IsZero() {
		client.CreatedAt = time.Now().UTC()
	}
	client.UpdatedAt = time.Now().UTC()
	obj := thunderClientObject(client, s.namespace())
	resource := s.Client.Resource(ThunderClientGVR).Namespace(s.namespace())
	current, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj.SetFinalizers([]string{thunderclient.Finalizer})
		_, err = resource.Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// An update replaces metadata wholesale, so carry over whatever finalizers
	// the resource already has and make sure ours is among them.
	obj.SetFinalizers(withFinalizer(current.GetFinalizers()))
	obj.SetResourceVersion(current.GetResourceVersion())
	_, err = resource.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

// withFinalizer returns finalizers including ours, without duplicating it.
func withFinalizer(finalizers []string) []string {
	for _, finalizer := range finalizers {
		if finalizer == thunderclient.Finalizer {
			return finalizers
		}
	}
	return append(append([]string(nil), finalizers...), thunderclient.Finalizer)
}

// withoutFinalizer returns finalizers with ours removed.
func withoutFinalizer(finalizers []string) []string {
	kept := make([]string, 0, len(finalizers))
	for _, finalizer := range finalizers {
		if finalizer != thunderclient.Finalizer {
			kept = append(kept, finalizer)
		}
	}
	return kept
}

// Delete releases the finalizer and removes the resource. The caller has
// already revoked the Thunder enrollment by this point, so holding the resource
// any longer would serve no purpose.
func (s *KubernetesThunderClientStore) Delete(ctx context.Context, claimUID types.UID) error {
	if s.Client == nil {
		return fmt.Errorf("dynamic client is required")
	}
	resource := s.Client.Resource(ThunderClientGVR).Namespace(s.namespace())
	name := ThunderClientName(claimUID)

	current, err := resource.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return ErrNotFound
	case err != nil:
		return err
	}

	if kept := withoutFinalizer(current.GetFinalizers()); len(kept) != len(current.GetFinalizers()) {
		updated := current.DeepCopy()
		updated.SetFinalizers(kept)
		if _, err := resource.Update(ctx, updated, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("release finalizer on ThunderClient %s: %w", name, err)
		}
	}

	err = resource.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *KubernetesThunderClientStore) namespace() string {
	if strings.TrimSpace(s.Namespace) == "" {
		return DefaultThunderClientNamespace
	}
	return strings.TrimSpace(s.Namespace)
}

func ThunderClientName(claimUID types.UID) string {
	name := strings.ToLower(strings.TrimSpace(string(claimUID)))
	name = strings.ReplaceAll(name, "_", "-")
	if name == "" {
		name = "unknown"
	}
	return "claim-" + name
}

func thunderClientObject(client ThunderClient, namespace string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "thundercompute.com/v1alpha1",
		"kind":       "ThunderClient",
		"metadata": map[string]any{
			"name":      ThunderClientName(client.ClaimUID),
			"namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/name": "thunder-dra-driver",
				claimUIDLabelName:        string(client.ClaimUID),
				gpuTypeLabelName:         strings.ToLower(client.GPUType),
				claimNamespaceLabelName:  client.ClaimNamespace,
			},
		},
		"spec": map[string]any{
			"claimUID":       string(client.ClaimUID),
			"claimNamespace": client.ClaimNamespace,
			"claimName":      client.ClaimName,
			"gpuType":        client.GPUType,
			"gpuCount":       client.GPUCount,
		},
		"status": map[string]any{
			"nodeName":          client.NodeName,
			"zone":              client.Zone,
			"requestName":       client.RequestName,
			"poolName":          client.PoolName,
			"deviceName":        client.DeviceName,
			"cdiName":           client.CDIName,
			"enrollmentTokenID": client.EnrollmentTokenID,
			"guestNamespace":    client.GuestNamespace,
			"guestSecretName":   client.GuestSecret,
			"createdAt":         client.CreatedAt.Format(time.RFC3339),
			"updatedAt":         client.UpdatedAt.Format(time.RFC3339),
		},
	}}
	if client.ShareID != nil {
		_, _, _ = unstructured.NestedMap(obj.Object, "status")
		_ = unstructured.SetNestedField(obj.Object, string(*client.ShareID), "status", "shareID")
	}
	if client.Consumer.Name != "" || client.Consumer.UID != "" {
		_ = unstructured.SetNestedMap(obj.Object, map[string]any{
			"apiGroup":  client.Consumer.APIGroup,
			"resource":  client.Consumer.Resource,
			"namespace": client.Consumer.Namespace,
			"name":      client.Consumer.Name,
			"uid":       string(client.Consumer.UID),
		}, "status", "consumer")
	}
	return obj
}

func thunderClientFromObject(obj *unstructured.Unstructured) *ThunderClient {
	client := &ThunderClient{}
	if obj == nil {
		return client
	}
	client.ClaimUID = types.UID(nestedString(obj, "spec", "claimUID"))
	client.ClaimNamespace = nestedString(obj, "spec", "claimNamespace")
	client.ClaimName = nestedString(obj, "spec", "claimName")
	client.GPUType = nestedString(obj, "spec", "gpuType")
	client.GPUCount = nestedInt64(obj, "spec", "gpuCount")
	client.NodeName = nestedString(obj, "status", "nodeName")
	client.Zone = nestedString(obj, "status", "zone")
	client.RequestName = nestedString(obj, "status", "requestName")
	client.PoolName = nestedString(obj, "status", "poolName")
	client.DeviceName = nestedString(obj, "status", "deviceName")
	client.CDIName = nestedString(obj, "status", "cdiName")
	client.EnrollmentTokenID = nestedString(obj, "status", "enrollmentTokenID")
	client.GuestNamespace = nestedString(obj, "status", "guestNamespace")
	client.GuestSecret = nestedString(obj, "status", "guestSecretName")
	if shareID := nestedString(obj, "status", "shareID"); shareID != "" {
		uid := types.UID(shareID)
		client.ShareID = &uid
	}
	if consumer, ok, _ := unstructured.NestedMap(obj.Object, "status", "consumer"); ok {
		client.Consumer = ResourceConsumer{
			APIGroup:  stringFromMap(consumer, "apiGroup"),
			Resource:  stringFromMap(consumer, "resource"),
			Namespace: stringFromMap(consumer, "namespace"),
			Name:      stringFromMap(consumer, "name"),
			UID:       types.UID(stringFromMap(consumer, "uid")),
		}
	}
	client.CreatedAt = nestedTime(obj, "status", "createdAt")
	client.UpdatedAt = nestedTime(obj, "status", "updatedAt")
	return client
}

func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value
}

func nestedInt64(obj *unstructured.Unstructured, fields ...string) int64 {
	value, _, _ := unstructured.NestedInt64(obj.Object, fields...)
	return value
}

func nestedTime(obj *unstructured.Unstructured, fields ...string) time.Time {
	value := nestedString(obj, fields...)
	if value == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func stringFromMap(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

const (
	ThunderGuestInstallScriptKey = "install-thunder-client.sh"
	ThunderGuestSecretTokenKey   = "enrollment-token"
	ThunderGuestTokenPath        = "/mnt/thunder-setup/enrollment-token"
)

type KubernetesGuestConfigStore struct {
	Client kubernetes.Interface
}

func NewKubernetesGuestConfigStore(client kubernetes.Interface) *KubernetesGuestConfigStore {
	return &KubernetesGuestConfigStore{Client: client}
}

func (s *KubernetesGuestConfigStore) Create(ctx context.Context, allocation Allocation, token string, installCommand string) (GuestArtifacts, error) {
	if s.Client == nil {
		return GuestArtifacts{}, fmt.Errorf("kubernetes client is required")
	}
	namespace := strings.TrimSpace(allocation.ClaimNamespace)
	if namespace == "" {
		return GuestArtifacts{}, fmt.Errorf("claim namespace is required")
	}
	if strings.TrimSpace(token) == "" {
		return GuestArtifacts{}, fmt.Errorf("client enrollment token is required")
	}

	artifacts := GuestArtifacts{
		Namespace:  namespace,
		SecretName: ThunderGuestSetupSecretName(allocation.ClaimName),
	}

	// The token and the script that spends it travel together in one Secret,
	// so a VM mounts one filesystem rather than two.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      artifacts.SecretName,
			Namespace: artifacts.Namespace,
			Labels:    guestArtifactLabels(allocation),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			ThunderGuestSecretTokenKey:   []byte(token),
			ThunderGuestInstallScriptKey: []byte(thunderGuestInstallScript(installCommand)),
		},
	}
	if err := upsertSecret(ctx, s.Client, secret); err != nil {
		return GuestArtifacts{}, err
	}
	return artifacts, nil
}

func (s *KubernetesGuestConfigStore) Remove(ctx context.Context, artifacts GuestArtifacts) error {
	if s.Client == nil {
		return fmt.Errorf("kubernetes client is required")
	}
	namespace := strings.TrimSpace(artifacts.Namespace)
	if namespace == "" {
		return nil
	}
	if err := deleteSecret(ctx, s.Client, namespace, artifacts.SecretName); err != nil {
		return err
	}
	return nil
}

// ThunderGuestSetupSecretName is the Secret a VM mounts to set itself up as
// a Thunder client.
func ThunderGuestSetupSecretName(claimName string) string {
	return thunderGuestArtifactName(claimName, "thunder-setup")
}

func thunderGuestArtifactName(claimName string, suffix string) string {
	base := strings.ToLower(strings.TrimSpace(claimName))
	if base == "" {
		base = "claim"
	}
	name := base + "-" + suffix
	if len(name) <= 253 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:])[:10]
	maxBaseLen := 253 - len(suffix) - len(hash) - 2
	return strings.TrimSuffix(base[:maxBaseLen], "-") + "-" + hash + "-" + suffix
}

func guestArtifactLabels(allocation Allocation) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "thunder-dra-driver",
		"app.kubernetes.io/component": "guest-config",
		claimUIDLabelName:             string(allocation.ClaimUID),
		claimNamespaceLabelName:       allocation.ClaimNamespace,
		claimNameLabelName:            allocation.ClaimName,
	}
}

// thunderGuestInstallScript is what a VM guest runs to become a Thunder client.
//
// It installs the Thunder client and nothing else. A client does not need an
// NVIDIA driver: libthunder intercepts CUDA in the guest and carries it to a
// GPU server over the network, and the driver lives on that server. Installing
// one here would pin the guest to a distribution and a driver version, and add
// a kernel module build to every VM boot, for something no workload uses.
func thunderGuestInstallScript(installCommand string) string {
	installCommand = strings.TrimSpace(installCommand)
	if installCommand == "" {
		installCommand = "echo thunder client install command is not configured >&2; exit 1"
	}
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

TOKEN_FILE="${THUNDER_ENROLLMENT_TOKEN_FILE:-%s}"
if [[ ! -s "${TOKEN_FILE}" ]]; then
  echo "Thunder enrollment token file is missing: ${TOKEN_FILE}" >&2
  exit 1
fi
export %s="$(cat "${TOKEN_FILE}")"

%s
`, ThunderGuestTokenPath, ThunderEnrollmentTokenEnv, installCommand)
}

func upsertSecret(ctx context.Context, client kubernetes.Interface, desired *corev1.Secret) error {
	resource := client.CoreV1().Secrets(desired.Namespace)
	current, err := resource.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = resource.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	updated := current.DeepCopy()
	updated.Labels = desired.Labels
	updated.Type = desired.Type
	updated.Data = desired.Data
	_, err = resource.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func deleteSecret(ctx context.Context, client kubernetes.Interface, namespace string, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	err := client.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

const (
	ThunderEnrollmentTokenEnv      = "THUNDER_ENROLLMENT_TOKEN"
	ThunderClientInstallCommandEnv = "THUNDER_CLIENT_INSTALL_COMMAND"
)

// cdiHookTimeoutSeconds is what the runtime allows the hook, kept above the
// hook's own cdiHookTimeout so the hook reports its failure rather than being
// killed mid-write.
const cdiHookTimeoutSeconds = 120

type FileCDIDeviceStore struct {
	SpecDir  string
	StateDir string
	Kind     string

	LibCUDAPath          string
	LibNVMLPath          string
	NVSMIPath            string
	ClientInstallCommand string

	// HookPath is the host executable the container runtime runs while
	// creating a container. Empty disables the hook, which leaves the
	// container to supply its own Thunder client.
	HookPath string
	// CacheDir is where the hook keeps this node's copy of libthunder.so.
	CacheDir string
	// The hook is told where to enrol and where to fetch the library, since
	// it runs as a bare process on the host with none of the daemon's config.
	CentralURL       string
	TelemetryURL     string
	InstallURL       string
	ArtifactBaseURL  string
	LibthunderURL    string
	LibthunderSHA256 string
	CABundlePath     string
}

func NewFileCDIDeviceStore(specDir string) *FileCDIDeviceStore {
	return &FileCDIDeviceStore{SpecDir: specDir, Kind: DefaultCDIKind}
}

func (s *FileCDIDeviceStore) Create(ctx context.Context, allocation Allocation, token string) (string, error) {
	kind := s.kind()
	deviceName := cdiDeviceName(allocation.ClaimUID)
	qualifiedName := kind + "=" + deviceName
	if strings.TrimSpace(s.ClientInstallCommand) == "" {
		return "", fmt.Errorf("client install command is required")
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("client enrollment token is required")
	}

	stateDir := s.stateDir(deviceName)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}
	// The hook reads the token from disk rather than taking it on a command
	// line, where it would be visible in the CDI spec and in host process
	// listings for as long as the container is being created.
	if err := os.WriteFile(s.tokenPath(deviceName), []byte(token+"\n"), 0600); err != nil {
		_ = os.RemoveAll(stateDir)
		return "", err
	}

	containerEdits := map[string]any{
		"env":    s.containerEnv(qualifiedName, allocation, token),
		"mounts": s.mounts(),
	}
	if hooks := s.hooks(deviceName, allocation); len(hooks) > 0 {
		containerEdits["hooks"] = hooks
	}
	spec := map[string]any{
		"cdiVersion": "0.6.0",
		"kind":       kind,
		"devices": []map[string]any{
			{
				"name":           deviceName,
				"containerEdits": containerEdits,
			},
		},
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		_ = os.RemoveAll(stateDir)
		return "", err
	}
	if err := os.MkdirAll(s.specDir(), 0755); err != nil {
		_ = os.RemoveAll(stateDir)
		return "", err
	}
	path := s.specPath(qualifiedName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		_ = os.RemoveAll(stateDir)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		_ = os.RemoveAll(stateDir)
		return "", err
	}
	return qualifiedName, nil
}

// StagedClientID reports the Thunder client the CDI hook enrolled for this
// device. The hook caches the exchanged config in the claim's state dir, and
// the client ID in it is the only record of the enrollment the daemon has: the
// exchange happens in the hook, on container create, long after prepare.
//
// An empty string means no container ever started for this claim, so no client
// was created and there is nothing to revoke.
func (s *FileCDIDeviceStore) StagedClientID(qualifiedName string) string {
	if strings.TrimSpace(qualifiedName) == "" {
		return ""
	}
	deviceName := cdiDeviceNameFromQualifiedName(qualifiedName)
	raw, err := os.ReadFile(filepath.Join(s.stateDir(deviceName), thunderConfigFile))
	if err != nil {
		return ""
	}
	var config ThunderClientConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return ""
	}
	return strings.TrimSpace(config.ClientID)
}

func (s *FileCDIDeviceStore) Remove(ctx context.Context, qualifiedName string) error {
	if strings.TrimSpace(qualifiedName) == "" {
		return nil
	}
	// Wait for any hook still staging this claim. Deleting the state dir
	// underneath one would fail that container with a confusing missing-token
	// error instead of letting it finish and be torn down cleanly.
	deviceName := cdiDeviceNameFromQualifiedName(qualifiedName)
	if unlock, err := lockStateDir(s.stateDir(deviceName)); err == nil {
		defer unlock()
	}
	specErr := os.Remove(s.specPath(qualifiedName))
	if specErr != nil && !os.IsNotExist(specErr) {
		return specErr
	}
	if err := os.RemoveAll(s.stateDir(deviceName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FileCDIDeviceStore) specDir() string {
	if strings.TrimSpace(s.SpecDir) == "" {
		return "/var/run/cdi"
	}
	return strings.TrimSpace(s.SpecDir)
}

func (s *FileCDIDeviceStore) stateRoot() string {
	if strings.TrimSpace(s.StateDir) != "" {
		return strings.TrimSpace(s.StateDir)
	}
	return filepath.Join(s.specDir(), "thunder")
}

func (s *FileCDIDeviceStore) kind() string {
	if strings.TrimSpace(s.Kind) == "" {
		return DefaultCDIKind
	}
	return strings.TrimSpace(s.Kind)
}

func (s *FileCDIDeviceStore) libCUDAPath() string {
	if strings.TrimSpace(s.LibCUDAPath) == "" {
		return DefaultLibCUDAPath
	}
	return strings.TrimSpace(s.LibCUDAPath)
}

func (s *FileCDIDeviceStore) libNVMLPath() string {
	if strings.TrimSpace(s.LibNVMLPath) == "" {
		return DefaultLibNVMLPath
	}
	return strings.TrimSpace(s.LibNVMLPath)
}

func (s *FileCDIDeviceStore) nvidiaSMIPath() string {
	if strings.TrimSpace(s.NVSMIPath) == "" {
		return DefaultNVSMIPath
	}
	return strings.TrimSpace(s.NVSMIPath)
}

func (s *FileCDIDeviceStore) specPath(qualifiedName string) string {
	name := strings.NewReplacer("/", "-", "=", "-", ":", "-", ".", "-").Replace(qualifiedName)
	return filepath.Join(s.specDir(), name+".json")
}

func (s *FileCDIDeviceStore) stateDir(deviceName string) string {
	return filepath.Join(s.stateRoot(), deviceName)
}

// tokenPath is where Create stages the enrollment token for the hook.
func (s *FileCDIDeviceStore) tokenPath(deviceName string) string {
	return filepath.Join(s.stateDir(deviceName), thunderTokenFile)
}

// hooks is the createRuntime hook that stages the Thunder client into the
// container being created. It runs before pivot_root, so it is the one place
// that can write into a container filesystem no matter what the image contains
// — no shell, no curl and no root required of the workload.
//
// createRuntime rather than createContainer: the two differ only in which
// namespaces they run in, and createContainer runs in the container's. That
// leaves the hook resolving DNS against the host's resolv.conf inside the
// container's empty network namespace, so every fetch fails with connection
// refused. createRuntime runs in the runtime's namespaces, where the node's
// network works, and still sees the rootfs at <bundle>/rootfs.
func (s *FileCDIDeviceStore) hooks(deviceName string, allocation Allocation) []map[string]any {
	hookPath := strings.TrimSpace(s.HookPath)
	if hookPath == "" {
		return nil
	}

	args := []string{hookPath, CDIHookCommand, "--state-dir", s.stateDir(deviceName)}
	for flag, value := range map[string]string{
		"--central-url":       s.CentralURL,
		"--telemetry-url":     s.TelemetryURL,
		"--install-url":       s.InstallURL,
		"--artifact-base-url": s.ArtifactBaseURL,
		"--libthunder-url":    s.LibthunderURL,
		"--libthunder-sha256": s.LibthunderSHA256,
		"--cache-dir":         s.CacheDir,
		"--ca-bundle":         s.CABundlePath,
		"--client-name":       thunderClientName(allocation),
	} {
		if strings.TrimSpace(value) != "" {
			args = append(args, flag, value)
		}
	}
	// Map iteration is unordered and this ends up in a file on disk, so sort
	// the flag pairs to keep a rewritten spec byte-identical.
	sortArgPairs(args[4:])

	return []map[string]any{
		{
			"hookName": "createRuntime",
			"path":     hookPath,
			"args":     args,
			"timeout":  cdiHookTimeoutSeconds,
		},
	}
}

// thunderClientName is what the enrollment is called in Thunder. The pod name
// makes an enrollment traceable to the workload holding it; the claim name is
// the fallback for a claim nothing has reserved yet.
func thunderClientName(allocation Allocation) string {
	if name := strings.TrimSpace(allocation.Consumer.Name); name != "" {
		return name
	}
	return strings.TrimSpace(allocation.ClaimName)
}

// sortArgPairs sorts a flat --flag/value slice by flag, keeping pairs together.
func sortArgPairs(args []string) {
	pairs := make([][2]string, 0, len(args)/2)
	for i := 0; i+1 < len(args); i += 2 {
		pairs = append(pairs, [2]string{args[i], args[i+1]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	for i, pair := range pairs {
		args[i*2], args[i*2+1] = pair[0], pair[1]
	}
}

func (s *FileCDIDeviceStore) containerEnv(qualifiedName string, allocation Allocation, token string) []string {
	env := []string{
		ThunderEnrollmentTokenEnv + "=" + token,
		ThunderClientInstallCommandEnv + "=" + strings.TrimSpace(s.ClientInstallCommand),
		"THUNDER_RESOURCE_CLAIM_UID=" + string(allocation.ClaimUID),
		"THUNDER_RESOURCE_CLAIM_NAMESPACE=" + allocation.ClaimNamespace,
		"THUNDER_RESOURCE_CLAIM_NAME=" + allocation.ClaimName,
		"THUNDER_GPU_TYPE=" + allocation.GPUType,
		"THUNDER_GPU_COUNT=" + fmt.Sprint(allocation.GPUCount),
		"THUNDER_CDI_DEVICE=" + qualifiedName,
		"LD_PRELOAD=/etc/thunder/libthunder.so",
	}
	pathDirs := uniqueNonEmpty([]string{
		filepath.Dir(s.nvidiaSMIPath()),
		"/usr/local/sbin",
		"/usr/local/bin",
		"/usr/sbin",
		"/usr/bin",
		"/sbin",
		"/bin",
	})
	libraryDirs := uniqueNonEmpty([]string{
		filepath.Dir(s.libCUDAPath()),
		filepath.Dir(s.libNVMLPath()),
	})
	if len(pathDirs) > 0 {
		env = append(env, "PATH="+strings.Join(pathDirs, ":"))
	}
	if len(libraryDirs) > 0 {
		env = append(env, "LD_LIBRARY_PATH="+strings.Join(libraryDirs, ":"))
	}
	return env
}

func (s *FileCDIDeviceStore) hookEnv(allocation Allocation, token string) []string {
	return []string{
		ThunderEnrollmentTokenEnv + "=" + token,
		"THUNDER_RESOURCE_CLAIM_UID=" + string(allocation.ClaimUID),
		"THUNDER_RESOURCE_CLAIM_NAMESPACE=" + allocation.ClaimNamespace,
		"THUNDER_RESOURCE_CLAIM_NAME=" + allocation.ClaimName,
		"THUNDER_GPU_TYPE=" + allocation.GPUType,
		"THUNDER_GPU_COUNT=" + fmt.Sprint(allocation.GPUCount),
	}
}

func (s *FileCDIDeviceStore) mounts() []map[string]any {
	return uniqueMounts([]map[string]any{
		bindMount(s.nvidiaSMIPath()),
		bindMount(s.libCUDAPath()),
		bindMount(s.libNVMLPath()),
	})
}

func bindMount(path string) map[string]any {
	return map[string]any{
		"hostPath":      path,
		"containerPath": path,
		"type":          "none",
		"options":       []string{"ro", "bind"},
	}
}

func uniqueMounts(mounts []map[string]any) []map[string]any {
	seen := map[string]struct{}{}
	unique := make([]map[string]any, 0, len(mounts))
	for _, mount := range mounts {
		hostPath, _ := mount["hostPath"].(string)
		containerPath, _ := mount["containerPath"].(string)
		if strings.TrimSpace(hostPath) == "" || strings.TrimSpace(containerPath) == "" {
			continue
		}
		key := hostPath + "\x00" + containerPath
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, mount)
	}
	return unique
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || value == "." {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func cdiDeviceName(claimUID types.UID) string {
	return ThunderClientName(claimUID)
}

func cdiDeviceNameFromQualifiedName(qualifiedName string) string {
	if index := strings.LastIndex(qualifiedName, "="); index >= 0 && index < len(qualifiedName)-1 {
		return qualifiedName[index+1:]
	}
	return strings.NewReplacer("/", "-", "=", "-", ":", "-", ".", "-").Replace(qualifiedName)
}
