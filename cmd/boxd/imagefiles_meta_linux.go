//go:build linux

package main

import (
	"io/fs"
	"os"
	"syscall"
)

// applyMetadata gives a copied entry its source's owner, mode, and file time.
// The mode is set after the owner: chown clears setuid and setgid.
func applyMetadata(target string, info fs.FileInfo) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if err := os.Lchown(target, int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	if err := os.Chmod(target, info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		_ = os.Chtimes(target, info.ModTime(), info.ModTime())
	}
	return nil
}
