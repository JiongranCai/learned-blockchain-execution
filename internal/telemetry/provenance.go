package telemetry

import (
	"os"
	"runtime"
	"runtime/debug"
)

const UpstreamCommit = "7afe924fb4a611a2626f92338f1f76e4ebefa62f"

func BuildIdentity() (revision string, modified bool) {
	revision = "unknown"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return revision, false
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

func CollectHardware() Hardware {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	cpuModel, memoryBytes, kernel := platformHardware()
	cpuAllowed, memoryAllowed, governor := platformControls()
	return Hardware{
		Hostname:          hostname,
		GOOS:              runtime.GOOS,
		GOARCH:            runtime.GOARCH,
		CPUModel:          cpuModel,
		LogicalCPUs:       runtime.NumCPU(),
		MemoryBytes:       memoryBytes,
		Kernel:            kernel,
		GoVersion:         runtime.Version(),
		GOMAXPROCS:        runtime.GOMAXPROCS(0),
		CPUAllowedList:    cpuAllowed,
		MemoryAllowedList: memoryAllowed,
		CPUGovernor:       governor,
	}
}
