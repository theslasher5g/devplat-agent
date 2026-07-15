package vmmanager

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// detectCPUTotal and detectRamTotalMb are declared as vars (not plain funcs)
// so manager_test.go can substitute deterministic values instead of
// depending on whatever machine the tests happen to run on.

// detectCPUTotal returns the number of logical CPUs available to this
// process — on bare-metal Ubuntu Server (no other workloads sharing the
// box), that's the host's real core count.
var detectCPUTotal = func() int64 {
	return int64(runtime.NumCPU())
}

// detectRamTotalMb parses /proc/meminfo for MemTotal. Falls back to 0 (which
// will make capacity math report zero slots, a loud and safe failure) if the
// host doesn't expose it for some reason.
var detectRamTotalMb = func() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
