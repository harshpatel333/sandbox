//go:build !linux

package main

import (
	"io/fs"
	"os"
)

func applyMetadata(target string, info fs.FileInfo) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	return os.Chmod(target, info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
}
