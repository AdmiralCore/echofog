//go:build !windows

package filexfer

import "golang.org/x/sys/unix"

// FreeSpace is how many bytes an unprivileged user may still write to the volume of dir.
func FreeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
