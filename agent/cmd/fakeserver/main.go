// Command fakeserver is a Plains of Pain stand-in for supervisor tests.
// It is not the game. It prints a dedicated-server boot log, including
// "Set Game State: RUNNING", and exits when it receives an interrupt.
//
// Behavior comes from fakeserver.json in the working directory. Launch
// arguments are ignored so the Plains of Pain argument list can be passed
// through unchanged.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func main() {
	cfg := loadConfig()
	out := bufio.NewWriter(os.Stdout)
	say := func(line string) {
		if _, err := fmt.Fprintln(out, line); err != nil {
			os.Exit(1)
		}
		if err := out.Flush(); err != nil {
			os.Exit(1)
		}
	}

	say("Dedicated server boot")
	say("Loading configs/server.json")
	if cfg.ExitBeforeReady {
		say("Set Game State: FAILED")
		os.Exit(1)
	}

	stop := make(chan struct{})
	go func() {
		waitForStop(cfg.IgnoreStop)
		close(stop)
	}()

	var crash <-chan time.Time
	if cfg.CrashAfter > 0 {
		crash = time.After(cfg.CrashAfter)
	}
	var ready <-chan time.Time
	if !cfg.Hang {
		ready = time.After(cfg.ReadyDelay)
	}

	for {
		// If the ready delay already elapsed, print it before a crash that
		// became due in the same wake. A blocked select would choose either.
		if ready != nil {
			select {
			case <-ready:
				ready = nil
				say("Set Game State: RUNNING")
				continue
			default:
			}
		}
		select {
		case <-ready:
			ready = nil
			say("Set Game State: RUNNING")
		case <-crash:
			say("fatal error")
			os.Exit(1)
		case <-stop:
			say("Stopping")
			return
		}
	}
}

type fileConfig struct {
	ReadyDelay      string `json:"readyDelay"`
	CrashAfter      string `json:"crashAfter"`
	Hang            bool   `json:"hang"`
	IgnoreStop      bool   `json:"ignoreStop"`
	ExitBeforeReady bool   `json:"exitBeforeReady"`
}

type config struct {
	ReadyDelay      time.Duration
	CrashAfter      time.Duration
	Hang            bool
	IgnoreStop      bool
	ExitBeforeReady bool
}

func loadConfig() config {
	cfg := config{ReadyDelay: 50 * time.Millisecond}
	body, err := os.ReadFile("fakeserver.json")
	if err != nil {
		return cfg
	}
	var file fileConfig
	if err := json.Unmarshal(body, &file); err != nil {
		exitf("fakeserver.json: %v", err)
	}
	if file.ReadyDelay != "" {
		d, err := time.ParseDuration(file.ReadyDelay)
		if err != nil {
			exitf("readyDelay: %v", err)
		}
		cfg.ReadyDelay = d
	}
	if file.CrashAfter != "" {
		d, err := time.ParseDuration(file.CrashAfter)
		if err != nil {
			exitf("crashAfter: %v", err)
		}
		cfg.CrashAfter = d
	}
	cfg.Hang = file.Hang
	cfg.IgnoreStop = file.IgnoreStop
	cfg.ExitBeforeReady = file.ExitBeforeReady
	return cfg
}

func exitf(format string, args ...any) {
	if _, err := fmt.Fprintf(os.Stderr, format+"\n", args...); err != nil {
		os.Exit(2)
	}
	os.Exit(2)
}
