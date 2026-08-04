package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
)

const UpstreamCommit = "7afe924fb4a611a2626f92338f1f76e4ebefa62f"

var binaryIdentity struct {
	sync.Once
	hash string
	err  error
}

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

func BinaryHash() (string, error) {
	binaryIdentity.Do(func() {
		path, err := os.Executable()
		if err != nil {
			binaryIdentity.err = err
			return
		}
		file, err := os.Open(path)
		if err != nil {
			binaryIdentity.err = err
			return
		}
		defer file.Close()
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			binaryIdentity.err = err
			return
		}
		binaryIdentity.hash = hex.EncodeToString(digest.Sum(nil))
	})
	return binaryIdentity.hash, binaryIdentity.err
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
