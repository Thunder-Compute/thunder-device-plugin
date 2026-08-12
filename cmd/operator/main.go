package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/operator"
	"github.com/Thunder-Compute/thunder-device-plugin/internal/version"
	thunder "github.com/Thunder-Compute/thunder-sdk"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var kubeconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig for out-of-cluster development")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))
	logger.Info("starting thunder-device-plugin-operator", "version", version.Get(), "revision", version.Revision())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := operator.ConfigFromEnv()
	if err != nil {
		logger.Error("invalid config", "error", err)
		os.Exit(1)
	}

	restConfig, err := kubernetesConfig(kubeconfig)
	if err != nil {
		logger.Error("build kubernetes config", "error", err)
		os.Exit(1)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logger.Error("build kubernetes client", "error", err)
		os.Exit(1)
	}

	// The operator reads and writes the ThunderClient custom resource, so it
	// needs a dynamic client alongside the typed one.
	clients, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error("build dynamic kubernetes client", "error", err)
		os.Exit(1)
	}

	thunderAPI, err := thunderClientFromEnv()
	if err != nil {
		logger.Error("build thunder api client", "error", err)
		os.Exit(1)
	}

	op := operator.New(cfg, kube, clients, thunderAPI, logger)
	if err := op.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("operator stopped", "error", err)
		os.Exit(1)
	}
}

func thunderClientFromEnv() (*thunder.Client, error) {
	apiToken := strings.TrimSpace(os.Getenv("THUNDER_API_TOKEN"))
	if apiToken == "" {
		return nil, fmt.Errorf("THUNDER_API_TOKEN is required")
	}
	return thunder.NewClient(os.Getenv("THUNDER_API_URL"), apiToken,
		thunder.WithUserAgent(version.UserAgent("operator"))), nil
}

func kubernetesConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if envKubeconfig := os.Getenv("KUBECONFIG"); envKubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", envKubeconfig)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".kube", "config")
		if _, err := os.Stat(candidate); err == nil {
			return clientcmd.BuildConfigFromFlags("", candidate)
		}
	}
	return rest.InClusterConfig()
}
