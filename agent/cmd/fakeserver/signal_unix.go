//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func waitForStop(ignore bool) {
	if ignore {
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		select {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}
