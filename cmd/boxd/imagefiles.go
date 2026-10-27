package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The guest mounts fresh tmpfs over /tmp and /run, hiding what the image ships
// there. Those mounts are memory, so only a small tree is copied into them.
const (
	imageDirMaxBytes   = 64 << 20
	imageDirMaxEntries = 10000
)

var errImageDirTooLarge = errors.New("image directory over the copy limit")

// imageDirWithinLimits walks src, stopping as soon as it passes either limit.
// Directories are read in batches, so even one huge directory is never read
// whole.
func imageDirWithinLimits(src string) error {
	var bytes int64
	entries := 0
	var walk func(dir string) error
	walk = func(dir string) error {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			batch, err := f.ReadDir(256)
			for _, d := range batch {
				if entries++; entries > imageDirMaxEntries {
					return errImageDirTooLarge
				}
				switch {
				case d.IsDir():
					if err := walk(filepath.Join(dir, d.Name())); err != nil {
						return err
					}
				case d.Type().IsRegular():
					info, err := d.Info()
					if err != nil {
						return err
					}
					if bytes += info.Size(); bytes > imageDirMaxBytes {
						return errImageDirTooLarge
					}
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	return walk(src)
}

// copyImageDir copies src into dst, keeping modes, owners, symlinks, and file
// times, and leaving anything already at a destination path alone. Sockets,
// pipes, and devices are skipped: they mean nothing in a fresh boot.
func copyImageDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dst, rel)
		if existing, err := os.Lstat(target); err == nil {
			if d.IsDir() && !existing.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyRegularFile(path, target); err != nil {
				return err
			}
		default:
			return nil
		}
		return applyMetadata(target, info)
	})
}

func copyRegularFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// resolveImageDir follows a symlinked /tmp or /run inside the image root,
// refusing a link that would leave it.
func resolveImageDir(root, name string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || strings.HasPrefix(rel, "..") {
		return "", errors.New("image directory leaves the image root")
	}
	return resolved, nil
}
