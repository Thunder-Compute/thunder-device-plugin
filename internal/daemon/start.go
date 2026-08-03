package daemon

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type PluginConfig struct {
	DriverName       string
	NodeName         string
	KubeletPluginDir string
	RegistrarDir     string
}

func StartNodePlugin(ctx context.Context, driver *Driver, kube kubernetes.Interface, cfg PluginConfig) (*kubeletplugin.Helper, error) {
	if driver == nil {
		return nil, fmt.Errorf("driver is required")
	}
	driverName := cfg.DriverName
	if driverName == "" {
		driverName = DefaultDriverName
	}
	driver.DriverName = driverName
	if driver.Kube == nil {
		driver.Kube = kube
	}
	if driver.NodeName == "" {
		driver.NodeName = cfg.NodeName
	}

	options := []kubeletplugin.Option{
		kubeletplugin.DriverName(driverName),
		kubeletplugin.KubeClient(kube),
		kubeletplugin.NodeName(cfg.NodeName),
		kubeletplugin.Serialize(true),
		kubeletplugin.GRPCVerbosity(3),
	}
	if cfg.KubeletPluginDir != "" {
		options = append(options, kubeletplugin.PluginDataDirectoryPath(cfg.KubeletPluginDir))
	}
	if cfg.RegistrarDir != "" {
		options = append(options, kubeletplugin.RegistrarDirectoryPath(cfg.RegistrarDir))
	}

	helper, err := kubeletplugin.Start(ctx, driver, options...)
	if err != nil {
		return nil, fmt.Errorf("start DRA kubelet plugin: %w", err)
	}
	return helper, nil
}
