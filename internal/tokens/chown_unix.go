//go:build unix

package tokens

import (
	"os"
	"syscall"
)

// chownLike gives file the owner and group of info's file (best effort as non-root:
// keeping our own uid always works, changing the group may not).
func chownLike(file *os.File, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := file.Chown(int(st.Uid), int(st.Gid)); err != nil && os.Geteuid() == 0 {
		return err
	}
	return nil
}
