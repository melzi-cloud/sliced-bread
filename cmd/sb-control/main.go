// Command sb-control is the SlicedBread control plane. It runs inside a tenant cluster.
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/melzi-cloud/sliced-bread/internal/control"
	"github.com/melzi-cloud/sliced-bread/internal/guard"
	"github.com/melzi-cloud/sliced-bread/internal/kube"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if os.Getenv("SB_MODE") == "wait" {
		waitFor(os.Getenv("SB_WAIT_ADDR"), 60*time.Second)
		return
	}
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

// waitFor dials addr until it answers or the time is up. It always exits 0: the action
// that follows reports the real error if the app stays unreachable.
func waitFor(addr string, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(time.Second)
	}
	log.Printf("%s did not answer within %s", addr, limit)
}
