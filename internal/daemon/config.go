package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	EnvNode                = "NODE"
	EnvZone                = "ZONE"
	EnvAdvertisedIP        = "ADVERTISED_IP"
	EnvMinNVDriverVersion  = "MIN_DRIVER_VERSION"
	EnvThunderAPIURL       = "THUNDER_API_URL"
	EnvThunderAPIToken     = "THUNDER_API_TOKEN"
	EnvHostRoot            = "HOST_ROOT"
	EnvLibCUDAPath         = "LIBCUDA_PATH"
	EnvLibNVMLPath         = "LIBNVIDIA_ML_PATH"
	EnvNVSMIPath           = "NVIDIA_SMI_PATH"
	EnvZoneLabel           = "NODE_ZONE_LABEL"
	EnvAdvertisedIPLabel   = "NODE_ADVERTISED_IP_LABEL"
	EnvHostTargetPID       = "HOST_TARGET_PID"
	EnvDRAEnabled          = "DRA_ENABLED"
	EnvDRADriverName       = "DRA_DRIVER_NAME"
	EnvCDISpecDir          = "CDI_SPEC_DIR"
	EnvThunderClientNS     = "THUNDER_CLIENT_NAMESPACE"
	EnvKubeletPluginDir    = "KUBELET_PLUGIN_DIR"
	EnvKubeletRegistrarDir = "KUBELET_REGISTRAR_DIR"
)

const (
	DefaultHostRoot            = "/host"
	DefaultLibCUDAPath         = "/usr/lib/x86_64-linux-gnu/libcuda.so.1"
	DefaultLibNVMLPath         = "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"
	DefaultNVSMIPath           = "/usr/bin/nvidia-smi"
	DefaultZoneLabel           = "topology.kubernetes.io/zone"
	DefaultAdvertisedIPLabel   = "thundercompute.com/advertised-ip"
	DefaultHostTargetPID       = "1"
	DefaultDRAEnabled          = true
	DefaultDRADriverName       = DefaultDriverName
	DefaultCDISpecDir          = "/var/run/cdi"
	DefaultThunderClientNS     = DefaultThunderClientNamespace
	DefaultKubeletPluginDir    = "/var/lib/kubelet/plugins/" + DefaultDriverName
	DefaultKubeletRegistrarDir = "/var/lib/kubelet/plugins_registry"
)

type Config struct {
	Node string
	Zone string
	// AdvertisedIP is the address Thunder clients use to reach this node.
	// When empty it is resolved from AdvertisedIPLabel and then from the
	// node's own IP. See resolveNodeAttributes.
	AdvertisedIP        string
	MinDriverVersion    string
	ThunderAPIURL       string
	ThunderAPIToken     string
	HostRoot            string
	LibCUDAPath         string
	LibNVMLPath         string
	NVSMIPath           string
	ZoneLabel           string
	AdvertisedIPLabel   string
	HostTargetPID       string
	DRAEnabled          bool
	DRADriverName       string
	CDISpecDir          string
	ThunderClientNS     string
	KubeletPluginDir    string
	KubeletRegistrarDir string
}

func ConfigFromEnv() (Config, error) {
	return configFromLookup(os.LookupEnv)
}

func configFromLookup(lookup func(string) (string, bool)) (Config, error) {
	node, err := requiredEnv(lookup, EnvNode)
	if err != nil {
		return Config{}, err
	}
	zone := optionalEnv(lookup, EnvZone, "")
	advertisedIP := optionalEnv(lookup, EnvAdvertisedIP, "")
	minVersion, err := requiredEnv(lookup, EnvMinNVDriverVersion)
	if err != nil {
		return Config{}, err
	}
	apiToken, err := requiredEnv(lookup, EnvThunderAPIToken)
	if err != nil {
		return Config{}, err
	}
	draEnabled, err := optionalBoolEnv(lookup, EnvDRAEnabled, DefaultDRAEnabled)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Node:                node,
		Zone:                zone,
		AdvertisedIP:        advertisedIP,
		MinDriverVersion:    minVersion,
		ThunderAPIURL:       optionalEnv(lookup, EnvThunderAPIURL, ""),
		ThunderAPIToken:     apiToken,
		HostRoot:            optionalEnv(lookup, EnvHostRoot, DefaultHostRoot),
		LibCUDAPath:         optionalEnv(lookup, EnvLibCUDAPath, DefaultLibCUDAPath),
		LibNVMLPath:         optionalEnv(lookup, EnvLibNVMLPath, DefaultLibNVMLPath),
		NVSMIPath:           optionalEnv(lookup, EnvNVSMIPath, DefaultNVSMIPath),
		ZoneLabel:           optionalEnv(lookup, EnvZoneLabel, DefaultZoneLabel),
		AdvertisedIPLabel:   optionalEnv(lookup, EnvAdvertisedIPLabel, DefaultAdvertisedIPLabel),
		HostTargetPID:       optionalEnv(lookup, EnvHostTargetPID, DefaultHostTargetPID),
		DRAEnabled:          draEnabled,
		DRADriverName:       optionalEnv(lookup, EnvDRADriverName, DefaultDRADriverName),
		CDISpecDir:          optionalEnv(lookup, EnvCDISpecDir, DefaultCDISpecDir),
		ThunderClientNS:     optionalEnv(lookup, EnvThunderClientNS, DefaultThunderClientNS),
		KubeletPluginDir:    optionalEnv(lookup, EnvKubeletPluginDir, DefaultKubeletPluginDir),
		KubeletRegistrarDir: optionalEnv(lookup, EnvKubeletRegistrarDir, DefaultKubeletRegistrarDir),
	}, nil
}

func requiredEnv(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be specified", key)
	}
	return strings.TrimSpace(value), nil
}

func optionalEnv(lookup func(string) (string, bool), key string, fallback string) string {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func optionalBoolEnv(lookup func(string) (string, bool), key string, fallback bool) (bool, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return parsed, nil
}
