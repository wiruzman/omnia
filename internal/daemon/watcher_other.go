//go:build !darwin || !cgo

package daemon

import "fmt"

func startNativeWatcher(_ []string, _ *fileWatcher) (func(), error) {
	return nil, fmt.Errorf("filesystem watcher requires macOS and cgo")
}
