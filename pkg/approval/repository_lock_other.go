//go:build !darwin && !linux

package approval

import (
	"errors"
	"os"
)

func openRepositoryLock(string, bool) (*os.File, error) {
	return nil, errors.New("repository cache leases require Linux or macOS")
}

func tryRepositoryLock(*os.File) (bool, error) {
	return false, errors.New("repository cache leases require Linux or macOS")
}
