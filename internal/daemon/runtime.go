package daemon

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	thunder "github.com/Thunder-Compute/thunder-sdk"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/version"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func Run(ctx context.Context, cfg Config) error {
	return run(ctx, cfg, osCommandRunner{targetPID: cfg.HostTargetPID}, kubernetesNodeInfoReader{})
}

func run(ctx context.Context, cfg Config, runner commandRunner, nodes nodeInfoReader) error {
	var err error
	cfg, err = resolveNodeAttributes(ctx, cfg, nodes)
	if err != nil {
		return err
	}
	log.Printf("starting thunder daemon %s (%s): node=%s zone=%s advertising=%s",
		version.Get(), version.Revision(), cfg.Node, cfg.Zone, cfg.AdvertisedIP)

	client := thunder.NewClient(cfg.ThunderAPIURL, cfg.ThunderAPIToken,
		thunder.WithUserAgent(version.UserAgent("daemon")))
	zoneID, err := ensureThunderZone(ctx, client, cfg.Zone)
	if err != nil {
		return err
	}
	log.Printf("resolved thunder zone: kubernetesZone=%s thunderZoneId=%s", cfg.Zone, zoneID)

	status, err := getThunderStatus(ctx, runner)
	if err != nil {
		log.Printf("thunder status unavailable before setup: %v", err)
	} else {
		logThunderStatus("initial", status)
		if status.Healthy {
			log.Printf("thunder is already healthy on node %s; skipping enrollment and installer", cfg.Node)
			if err := startDRAPlugin(ctx, cfg, client, zoneID); err != nil {
				return err
			}
			return monitorThunderStatus(ctx, runner, ThunderStatusInterval)
		}
	}

	gpuCount, driverVersion, err := nvidiaChecks(ctx, cfg, runner)
	if err != nil {
		return err
	}
	log.Printf("nvidia checks passed: driver=%s physical_gpus=%d", driverVersion, gpuCount)

	token, err := client.CreateServerEnrollment(ctx, thunder.CreateServerEnrollmentRequest{
		ZoneID: zoneID,
	})
	if err != nil {
		return fmt.Errorf("create thunder node enrollment: %w", err)
	}

	command := client.ServerEnrollmentCommand(thunder.ServerEnrollmentCommandRequest{
		EnrollmentToken: token.EnrollmentToken,
		IP:              cfg.AdvertisedIP,
		Zone:            cfg.Zone,
		ServerName:      cfg.Node,
	})
	if err := runner.RunShell(ctx, command); err != nil {
		return fmt.Errorf("run thunder node setup: %w", err)
	}

	log.Printf("thunder node setup completed: node=%s enrollmentTokenId=%s", cfg.Node, token.EnrollmentTokenID)
	if err := startDRAPlugin(ctx, cfg, client, zoneID); err != nil {
		return err
	}
	return monitorThunderStatus(ctx, runner, ThunderStatusInterval)
}

func startDRAPlugin(ctx context.Context, cfg Config, thunderClient *thunder.Client, zoneID string) error {
	if !cfg.DRAEnabled {
		log.Printf("DRA kubelet plugin disabled")
		return nil
	}
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("build in-cluster kubernetes config for DRA plugin: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build kubernetes client for DRA plugin: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("build dynamic kubernetes client for DRA plugin: %w", err)
	}
	cdiStore := NewFileCDIDeviceStore(cfg.CDISpecDir)
	cdiStore.StateDir = filepath.Join(cfg.KubeletPluginDir, "cdi-state")
	cdiStore.LibCUDAPath = cfg.LibCUDAPath
	cdiStore.LibNVMLPath = cfg.LibNVMLPath
	cdiStore.NVSMIPath = cfg.NVSMIPath
	cdiStore.ClientInstallCommand = thunderClient.ClientEnrollmentCommandFor(thunder.ClientEnrollmentCommandRequest{
		EnrollmentTokenEnv: ThunderEnrollmentTokenEnv,
	})

	driver := &Driver{
		DriverName: cfg.DRADriverName,
		NodeName:   cfg.Node,
		Kube:       kube,
		Tokens: ThunderTokenIssuer{
			Client: thunderClient,
			ZoneID: zoneID,
		},
		Clients: NewKubernetesThunderClientStore(dynamicClient, cfg.ThunderClientNS),
		CDI:     cdiStore,
		Guest:   NewKubernetesGuestConfigStore(kube),
	}
	_, err = StartNodePlugin(ctx, driver, kube, PluginConfig{
		DriverName:       cfg.DRADriverName,
		NodeName:         cfg.Node,
		KubeletPluginDir: cfg.KubeletPluginDir,
		RegistrarDir:     cfg.KubeletRegistrarDir,
	})
	if err != nil {
		return err
	}
	log.Printf("started DRA kubelet plugin: driver=%s node=%s cdiSpecDir=%s", cfg.DRADriverName, cfg.Node, cfg.CDISpecDir)
	return nil
}

// resolveNodeAttributes fills in the zone and advertised IP that were not set
// through the environment. The zone falls back to a node label; the advertised
// IP falls back to a node label and then to the node's own IP.
func resolveNodeAttributes(ctx context.Context, cfg Config, nodes nodeInfoReader) (Config, error) {
	if cfg.Zone != "" && cfg.AdvertisedIP != "" {
		return cfg, nil
	}

	node, err := nodes.Node(ctx, cfg.Node)
	if err != nil {
		return Config{}, fmt.Errorf("read kubernetes node %s: %w", cfg.Node, err)
	}
	if cfg.Zone == "" {
		cfg.Zone = strings.TrimSpace(node.Labels[cfg.ZoneLabel])
		if cfg.Zone == "" {
			return Config{}, fmt.Errorf("%s must be specified or node %s must have label %s", EnvZone, cfg.Node, cfg.ZoneLabel)
		}
	}
	if cfg.AdvertisedIP == "" {
		cfg.AdvertisedIP = firstNonEmpty(node.Labels[cfg.AdvertisedIPLabel], node.NodeIP())
		if cfg.AdvertisedIP == "" {
			return Config{}, fmt.Errorf("%s must be specified, or node %s must have label %s or an IP address in status.addresses", EnvAdvertisedIP, cfg.Node, cfg.AdvertisedIPLabel)
		}
	}
	return cfg, nil
}

func ensureThunderZone(ctx context.Context, client *thunder.Client, displayName string) (string, error) {
	zoneID, found, err := findSmallestZoneID(ctx, client, displayName)
	if err != nil {
		return "", fmt.Errorf("list thunder zones: %w", err)
	}
	if found {
		return zoneID, nil
	}

	if _, err := client.CreateZone(ctx, thunder.CreateZoneRequest{DisplayName: displayName}); err != nil && !thunder.IsConflict(err) {
		return "", fmt.Errorf("create thunder zone %q: %w", displayName, err)
	}

	zoneID, found, err = findSmallestZoneID(ctx, client, displayName)
	if err != nil {
		return "", fmt.Errorf("list thunder zones after create: %w", err)
	}
	if !found {
		return "", fmt.Errorf("thunder zone %q was not found after create", displayName)
	}
	return zoneID, nil
}

func findSmallestZoneID(ctx context.Context, client *thunder.Client, displayName string) (string, bool, error) {
	zones, err := client.ListZones(ctx)
	if err != nil {
		return "", false, err
	}

	smallest := ""
	for _, zone := range zones {
		if zone.DisplayName != displayName {
			continue
		}
		if smallest == "" || zone.ZoneID < smallest {
			smallest = zone.ZoneID
		}
	}
	return smallest, smallest != "", nil
}
