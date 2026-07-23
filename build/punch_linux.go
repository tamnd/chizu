//go:build linux

package build

import "syscall"

// Linux fallocate mode bits; the stdlib syscall package has Fallocate
// but not the flag constants.
const (
	fallocKeepSize  = 0x1
	fallocPunchHole = 0x2
)

// punchHole deallocates [off, off+length) of an open file while the
// logical size stays put, so consumed spool prefixes stop occupying
// disk mid-merge. Filesystems without hole support make this fail;
// callers treat failure as a lost optimization, not an error.
func punchHole(fd uintptr, off, length int64) error {
	return syscall.Fallocate(int(fd), fallocPunchHole|fallocKeepSize, off, length)
}
