//go:build !linux

package build

import "errors"

// punchHole is Linux-only; elsewhere the space comes back when the
// file is removed, which every spool is soon after.
func punchHole(fd uintptr, off, length int64) error {
	return errors.ErrUnsupported
}
