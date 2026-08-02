//go:build darwin

package telemetry

import "syscall"

func platformHardware() (string, uint64, string) {
	cpu, err := syscall.Sysctl("machdep.cpu.brand_string")
	if err != nil || cpu == "" {
		cpu = "unknown"
	}
	kernel, err := syscall.Sysctl("kern.osrelease")
	if err != nil || kernel == "" {
		kernel = "unknown"
	}
	return cpu, 0, kernel
}

func MaxRSSBytes() uint64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil || usage.Maxrss < 0 {
		return 0
	}
	return uint64(usage.Maxrss)
}

func platformControls() (string, string, string) {
	return "unavailable", "unavailable", "unavailable"
}
