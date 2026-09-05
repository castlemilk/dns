//go:build !unix

package config

// sameFilesystem cannot be answered portably outside unix. The caller treats an
// unknown answer as "not proven to be the same volume" and lets the deployment
// (the Helm chart mounts a dedicated emptyDir) enforce the separation.
func sameFilesystem(string, string) (bool, bool) { return false, false }
