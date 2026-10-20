//go:build !darwin || !cgo

package backend

import "fmt"

func newPlatformResourceBackend(string) (ResourceBackend, error) {
	return nil, fmt.Errorf("macOS resource APIs are unavailable; resource monitoring requires macOS with native API support")
}
