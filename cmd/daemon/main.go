package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Thunder-Compute/thunder-device-plugin/internal/daemon"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The same binary is the CDI hook. The container runtime runs it on the
	// host while creating a container, handing it the OCI state on stdin.
	if len(os.Args) > 1 && os.Args[1] == daemon.CDIHookCommand {
		opts, err := daemon.ParseCDIHookArgs(os.Args[2:])
		if err != nil {
			log.Fatalf("%s: %v", daemon.CDIHookCommand, err)
		}
		if err := daemon.RunCDIHook(ctx, opts, os.Stdin); err != nil {
			log.Fatalf("%s: %v", daemon.CDIHookCommand, err)
		}
		return
	}

	cfg, err := daemon.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := daemon.Run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}
