package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"thunder-device-plugin/internal/daemon"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := daemon.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := daemon.Run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}
