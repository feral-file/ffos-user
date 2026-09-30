// Package main exposes the production portal handler inside the network lab.
// It has no Wi-Fi controller and must only run in the isolated test namespace.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
)

func main() {
	server := portal.NewServer(portal.Config{Addr: "0.0.0.0:80", APSSID: "FF1-Lab"})
	if err := server.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("ready")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = server.Stop(shutdown)
}
