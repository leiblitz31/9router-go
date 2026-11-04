package providers

import (
	"runtime"
)

// nodePlatform is the Go runtime's platform in the spelling Node's
// process.platform uses, which is what upstream sends: "win32", not
// "windows".
func nodePlatform() string {
	return nodePlatformFor(runtime.GOOS)
}

// nodePlatformFor maps a Go OS name onto Node's process.platform spelling.
// It is a function of its argument so the mapping can be tested on a host
// that is not the one it describes.
func nodePlatformFor(goos string) string {
	if goos == "windows" {
		return "win32"
	}
	return goos
}

// NodePlatform is the platform name in Node's process.platform spelling
// ("win32" on Windows). Shared with the executors, whose envelopes carry the
// same field.
func NodePlatform() string {
	return nodePlatform()
}
