package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var ThunderClientGVR = schema.GroupVersionResource{
	Group:    "thundercompute.com",
	Version:  "v1alpha1",
	Resource: "clients",
}

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
		_, err = resource.Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(current.GetResourceVersion())
	_, err = resource.Update(ctx, obj, metav1.UpdateOptions{})
	return err
}

func (s *KubernetesThunderClientStore) Delete(ctx context.Context, claimUID types.UID) error {
	if s.Client == nil {
		return fmt.Errorf("dynamic client is required")
	}
	err := s.Client.Resource(ThunderClientGVR).Namespace(s.namespace()).Delete(ctx, ThunderClientName(claimUID), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
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
				"app.kubernetes.io/name":                  "thunder-dra-driver",
				"vgpu.thundercompute.com/claim-uid":       string(client.ClaimUID),
				"vgpu.thundercompute.com/gpu-type":        strings.ToLower(client.GPUType),
				"vgpu.thundercompute.com/claim-namespace": client.ClaimNamespace,
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

const ThunderEnrollmentTokenEnv = "THUNDER_ENROLLMENT_TOKEN"

const cdiHookTimeoutSeconds = 300

type FileCDIDeviceStore struct {
	SpecDir string
	Kind    string

	LibCUDAPath          string
	LibNVMLPath          string
	NVSMIPath            string
	ClientInstallCommand string
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
	tokenPath := s.tokenPath(deviceName)
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		_ = os.RemoveAll(stateDir)
		return "", err
	}
	hookPath := s.hookPath(deviceName)
	if err := os.WriteFile(hookPath, []byte(s.hookScript(tokenPath)), 0700); err != nil {
		_ = os.RemoveAll(stateDir)
		return "", err
	}

	spec := map[string]any{
		"cdiVersion": "0.6.0",
		"kind":       kind,
		"devices": []map[string]any{
			{
				"name": deviceName,
				"containerEdits": map[string]any{
					"env":    s.containerEnv(qualifiedName, allocation),
					"mounts": s.mounts(),
					"hooks": []map[string]any{
						{
							"hookName": "createContainer",
							"path":     hookPath,
							"args":     []string{hookPath},
							"env":      s.hookEnv(allocation),
							"timeout":  cdiHookTimeoutSeconds,
						},
					},
				},
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

func (s *FileCDIDeviceStore) Remove(ctx context.Context, qualifiedName string) error {
	if strings.TrimSpace(qualifiedName) == "" {
		return nil
	}
	specErr := os.Remove(s.specPath(qualifiedName))
	if specErr != nil && !os.IsNotExist(specErr) {
		return specErr
	}
	if err := os.RemoveAll(s.stateDir(cdiDeviceNameFromQualifiedName(qualifiedName))); err != nil && !os.IsNotExist(err) {
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
	return filepath.Join(s.specDir(), "thunder", deviceName)
}

func (s *FileCDIDeviceStore) tokenPath(deviceName string) string {
	return filepath.Join(s.stateDir(deviceName), "enrollment-token")
}

func (s *FileCDIDeviceStore) hookPath(deviceName string) string {
	return filepath.Join(s.stateDir(deviceName), "install-client.sh")
}

func (s *FileCDIDeviceStore) hookScript(tokenPath string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
TOKEN_FILE=%s
cleanup() {
  rm -f "$TOKEN_FILE"
}
trap cleanup EXIT
if [ ! -s "$TOKEN_FILE" ]; then
  exit 0
fi
%s="$(cat "$TOKEN_FILE")"
export %s
%s
`, shellQuote(tokenPath), ThunderEnrollmentTokenEnv, ThunderEnrollmentTokenEnv, strings.TrimSpace(s.ClientInstallCommand))
}

func (s *FileCDIDeviceStore) containerEnv(qualifiedName string, allocation Allocation) []string {
	env := []string{
		"THUNDER_RESOURCE_CLAIM_UID=" + string(allocation.ClaimUID),
		"THUNDER_RESOURCE_CLAIM_NAMESPACE=" + allocation.ClaimNamespace,
		"THUNDER_RESOURCE_CLAIM_NAME=" + allocation.ClaimName,
		"THUNDER_GPU_TYPE=" + allocation.GPUType,
		"THUNDER_GPU_COUNT=" + fmt.Sprint(allocation.GPUCount),
		"THUNDER_CDI_DEVICE=" + qualifiedName,
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

func (s *FileCDIDeviceStore) hookEnv(allocation Allocation) []string {
	return []string{
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
