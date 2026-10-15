//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func sampleResources(pid int, prev resourcePoint) (mem uint64, memOK bool, cpu float64, cpuOK bool, next resourcePoint) {
	if pid <= 0 {
		return 0, false, 0, false, resourcePoint{}
	}
	mem, memOK = readRSS(pid)
	proc, procOK := readProcTicks(pid)
	total, totalOK := readTotalTicks()
	next = resourcePoint{proc: proc, total: total, ok: procOK && totalOK}
	if prev.ok && next.ok && total > prev.total && proc >= prev.proc {
		cpu = float64(proc-prev.proc) / float64(total-prev.total) * float64(runtime.NumCPU()) * 100
		cpuOK = true
	}
	return mem, memOK, cpu, cpuOK, next
}

func readRSS(pid int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

func readProcTicks(pid int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	text := string(b)
	end := strings.LastIndex(text, ")")
	if end < 0 || end+2 >= len(text) {
		return 0, false
	}
	fields := strings.Fields(text[end+2:])
	// After comm: state, ppid, pgrp, session, tty, tpgid, flags,
	// minflt, cminflt, majflt, cmajflt, utime, stime.
	if len(fields) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return utime + stime, true
}

func readTotalTicks() (uint64, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "cpu" {
		return 0, false
	}
	var sum uint64
	for _, field := range fields[1:] {
		n, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, false
		}
		sum += n
	}
	return sum, true
}
