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
	EnvThunderPortRange    = "THUNDER_PORT_RANGE"
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
	EnvThunderInstallURL   = "THUNDER_INSTALL_URL"
	EnvThunderTelemetryURL = "THUNDER_TELEMETRY_URL"
	EnvArtifactBaseURL     = "THUNDER_ARTIFACT_BASE_URL"
	EnvLibthunderURL       = "LIBTHUNDER_URL"
	EnvLibthunderSHA256    = "LIBTHUNDER_SHA256"
	EnvCABundlePath        = "CA_BUNDLE_PATH"
	EnvThunderdLogUnit     = "THUNDERD_LOG_UNIT"
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
	// DefaultThunderInstallURL is the installer the CDI hook reads the
	// pinned libthunder.so digest out of. It is never executed.
	DefaultThunderInstallURL   = "https://get.thundercompute.com/install.sh"
	DefaultThunderTelemetryURL = "https://telemetry.thundercompute.com:2096"
	// DefaultCABundlePath is the node trust store staged into containers
	// that ship none of their own.
	DefaultCABundlePath = "/etc/ssl/certs/ca-certificates.crt"
	// DefaultThunderdLogUnit is the systemd unit whose journal is republished
	// as this pod's log, whether thunderd was installed as a unit file or, as
	// the daemon installs it, transiently.
	DefaultThunderdLogUnit = "thunderd.service"
)

type Config struct {
	Node string
	Zone string
	// AdvertisedIP is the address Thunder clients use to reach this node.
	// When empty it is resolved from AdvertisedIPLabel and then from the
	// node's own IP. See resolveNodeAttributes.
	AdvertisedIP     string
	MinDriverVersion string
	// PortRange is the host data-port range, "start-end", that thunderd binds
	// for CUDA traffic: a data and a control port per attached session. It is
	// passed to the installer at enrollment. Empty leaves the installer's own
	// default in place. Operators should choose a range that clears both the
	// cluster's NodePort range and the kernel's ephemeral range. (by claude)
	PortRange           string
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
	// ThunderInstallURL is read, not run: the CDI hook takes the pinned
	// libthunder.so digest from it so a node stages the same build the
	// installer would have.
	ThunderInstallURL   string
	ThunderTelemetryURL string
	ArtifactBaseURL     string
	// LibthunderURL and LibthunderSHA256 pin the library explicitly and
	// skip the installer entirely.
	LibthunderURL    string
	LibthunderSHA256 string
	CABundlePath     string
	// ThunderdLogUnit is the systemd unit the daemon follows the journal of
	// and republishes as its own log. Set it to "off" to leave thunderd's logs
	// on the node.
	ThunderdLogUnit string
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
	portRange, err := optionalPortRangeEnv(lookup, EnvThunderPortRange)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Node:                node,
		Zone:                zone,
		AdvertisedIP:        advertisedIP,
		MinDriverVersion:    minVersion,
		PortRange:           portRange,
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
		ThunderInstallURL:   optionalEnv(lookup, EnvThunderInstallURL, DefaultThunderInstallURL),
		ThunderTelemetryURL: optionalEnv(lookup, EnvThunderTelemetryURL, DefaultThunderTelemetryURL),
		ArtifactBaseURL:     optionalEnv(lookup, EnvArtifactBaseURL, ""),
		LibthunderURL:       optionalEnv(lookup, EnvLibthunderURL, ""),
		LibthunderSHA256:    optionalEnv(lookup, EnvLibthunderSHA256, ""),
		CABundlePath:        optionalEnv(lookup, EnvCABundlePath, DefaultCABundlePath),
		ThunderdLogUnit:     optionalEnv(lookup, EnvThunderdLogUnit, DefaultThunderdLogUnit),
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

// optionalPortRangeEnv reads a "start-end" port range. A malformed one fails
// here, at startup, rather than reaching an enrollment that would leave the
// node bound to ports nobody chose. (by claude)
func optionalPortRangeEnv(lookup func(string) (string, bool), key string) (string, error) {
	value := optionalEnv(lookup, key, "")
	if value == "" {
		return "", nil
	}
	malformed := fmt.Errorf("%s must be a port range like 32000-32199, got %q", key, value)
	rawStart, rawEnd, found := strings.Cut(value, "-")
	if !found {
		return "", malformed
	}
	start, err := strconv.Atoi(rawStart)
	if err != nil {
		return "", malformed
	}
	end, err := strconv.Atoi(rawEnd)
	if err != nil {
		return "", malformed
	}
	if start < 1 || end > 65535 || start > end {
		return "", fmt.Errorf("%s must be a port range within 1-65535 with start <= end, got %q", key, value)
	}
	return value, nil
}
