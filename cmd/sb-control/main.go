// Command sb-control is the SlicedBread control plane. It runs inside a tenant cluster.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/melzi-cloud/sliced-bread/internal/control"
	"github.com/melzi-cloud/sliced-bread/internal/guard"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	k, err := kube.InCluster()
	if err != nil {
		log.Fatal(err)
	}
	if os.Getenv("SB_MODE") == "guard" {
		if err := (&guard.Guard{Kube: k}).Run(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	s, err := control.New(ctx, control.ConfigFromEnv(), k)
	if err != nil {
		log.Fatal(err)
	}
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
