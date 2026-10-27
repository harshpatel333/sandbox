//go:build linux

package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// imageRootMount is where boot briefly mounts the root filesystem again to
// read the image's own /tmp and /run under the tmpfs mounts that hide them.
const imageRootMount = "/dev/.image-root"

// memoryHostsPath backs /etc/hosts in memory, as a container runtime keeps it,
// so the hostname can be mapped on the /init path without a disk write. It is
// created before the image's /run is copied in, which never overwrites a
// path, so nothing from the image can take its place.
const memoryHostsPath = "/run/.boxd/hosts"

// prepareGuestFiles runs once per boot, before boxd serves anything. A
// sandbox resumed or created from a snapshot keeps the result in memory, so
// it runs only when a template is built and when a sandbox boots cold.
func prepareGuestFiles() {
	start := time.Now()
	defer func() { log.Printf("init: guest files prepared in %s", time.Since(start)) }()
	if err := keepHostsInMemory(); err != nil {
		log.Printf("init: keep /etc/hosts in memory: %v", err)
	}
	if err := restoreImageDirs(); err != nil {
		log.Printf("init: restore the image's /tmp and /run: %v", err)
	}
}

func restoreImageDirs() error {
	if err := os.MkdirAll(imageRootMount, 0o700); err != nil {
		return err
	}
	defer os.Remove(imageRootMount)
	if err := unix.Mount("/", imageRootMount, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	defer unix.Unmount(imageRootMount, unix.MNT_DETACH)

	for _, dir := range []string{"tmp", "run"} {
		src, err := resolveImageDir(imageRootMount, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			log.Printf("init: image /%s: %v", dir, err)
			continue
		}
		if err := imageDirWithinLimits(src); err != nil {
			log.Printf("init: image /%s not copied: %v", dir, err)
			continue
		}
		if err := copyImageDir(src, "/"+dir); err != nil {
			log.Printf("init: copy image /%s: %v", dir, err)
		}
	}
	return nil
}

// keepHostsInMemory bind-mounts a memory copy of /etc/hosts over it. Edits
// then last for the sandbox's life, through pause and resume, and a cold boot
// starts again from the template's file, as a container's /etc/hosts does.
func keepHostsInMemory() error {
	if inMemory, err := sameFilesystem(etcHostsPath, filepath.Dir(memoryHostsPath)); err == nil && inMemory {
		return nil
	}
	data, err := os.ReadFile(etcHostsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(etcHostsPath, nil, 0o644); err != nil {
			return err
		}
	}
	if err := os.Mkdir(filepath.Dir(memoryHostsPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(memoryHostsPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return unix.Mount(memoryHostsPath, etcHostsPath, "", unix.MS_BIND, "")
}

func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}
