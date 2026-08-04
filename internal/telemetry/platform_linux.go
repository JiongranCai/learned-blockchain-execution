//go:build linux

package telemetry

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func platformHardware() (string, uint64, string) {
	cpu := firstProcValue("/proc/cpuinfo", "model name")
	if cpu == "" {
		cpu = firstProcValue("/proc/cpuinfo", "Hardware")
	}
	if cpu == "" {
		cpu = "unknown"
	}
	memory := uint64(0)
	if value := firstProcValue("/proc/meminfo", "MemTotal"); value != "" {
		fields := strings.Fields(value)
		if len(fields) > 0 {
			if kib, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
				memory = kib * 1024
			}
		}
	}
	kernel := "unknown"
	if encoded, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		if value := strings.TrimSpace(string(encoded)); value != "" {
			kernel = value
		}
	}
	return cpu, memory, kernel
}

func firstProcValue(path, key string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		left, right, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.TrimSpace(left) == key {
			return strings.TrimSpace(right)
		}
	}
	return ""
}

func MaxRSSBytes() uint64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil || usage.Maxrss < 0 {
		return 0
	}
	return uint64(usage.Maxrss) * 1024
}

func platformControls() (string, string, string) {
	cpuAllowed := firstProcValue("/proc/self/status", "Cpus_allowed_list")
	if cpuAllowed == "" {
		cpuAllowed = "unknown"
	}
	memoryAllowed := firstProcValue("/proc/self/status", "Mems_allowed_list")
	if memoryAllowed == "" {
		memoryAllowed = "unknown"
	}
	governor := "unknown"
	if encoded, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"); err == nil {
		if value := strings.TrimSpace(string(encoded)); value != "" {
			governor = value
		}
	}
	return cpuAllowed, memoryAllowed, governor
}
