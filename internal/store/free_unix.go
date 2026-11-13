//go:build linux || darwin

package store

import "syscall"

// FreeBytes reports free space on the disk that holds the data directory.
func (s Store) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.Root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
