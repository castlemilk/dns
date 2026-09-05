//go:build !unix

package hosting

// freeBytes cannot be answered portably outside unix. The caller treats an
// unknown answer as "do not refuse on free space"; the deployment (a dedicated
// emptyDir sized four times the per-upload cap) is the real guard.
func freeBytes(string) (uint64, bool) { return 0, false }
