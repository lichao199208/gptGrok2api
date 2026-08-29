//go:build windows

package httpapi

// Windows builds keep the image statistics available; disk capacity is filled
// by the server-side implementation on Linux deployments.
func diskUsage(string) (total, used, free uint64) {
	return 0, 0, 0
}
