package session

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// bytesPerSession is the RAM budget assumed per active session: an agent
// container plus a preview compose stack (open item O1).
const bytesPerSession = 4 << 30

// DefaultMaxActive derives the concurrency cap from total RAM.
func DefaultMaxActive() int {
	return capFor(totalRAM())
}

func capFor(ram uint64) int {
	if ram == 0 {
		return 2
	}
	return max(1, int(min(ram/bytesPerSession, 1024))) //nolint:gosec // bounded to 1024
}

// totalRAM returns physical memory in bytes, 0 when unknown.
func totalRAM() uint64 {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					return kb << 10
				}
			}
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err == nil {
			n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return 0
}
