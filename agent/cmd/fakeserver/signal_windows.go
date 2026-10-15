//go:build windows

package main

import (
	"os"
	"os/signal"
)

// Windows has no SIGTERM delivery through os/signal for a normal process.
// The supervisor's unverified fallback is TerminateProcess, which this
// stand-in cannot catch. Interrupt is the only signal Go surfaces here.
func waitForStop(ignore bool) {
	if ignore {
		signal.Ignore(os.Interrupt)
		select {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
}
