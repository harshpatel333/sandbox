package builder

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// pullAndFlatten resolves imageRef (e.g. "python:3.11") to a digest, pulls
// the linux/amd64 layers, and extracts the flattened filesystem into destDir.
// Returns the resolved "sha256:..." digest for persistence on template_build.
//
// destDir must exist and be writable. Existing contents are NOT cleared;
// callers should pass a fresh empty directory.
//
// Whiteout handling: OCI layers express deletions via `.wh.<name>` entries.
// We honor them during layer-by-layer extraction — a `.wh.foo` file in a
// later layer removes `foo` from the accumulated tree. This matches what a
// runtime like Docker/containerd does when materializing an image.
func (b *inProcessBuilder) pullAndFlatten(ctx context.Context, imageRef, destDir string) (string, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", imageRef, err)
	}

	// remote.WithContext lets context cancellation propagate into the HTTP
	// transport so aborted builds actually stop downloading.
	img, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithPlatform(v1.Platform{OS: b.cfg.PlatformOS, Architecture: b.cfg.PlatformArch}),
		remote.WithAuthFromKeychain(keychain),
	)
	if err != nil {
		return "", fmt.Errorf("fetch image %q: %w", imageRef, err)
	}

	if err := validatePlatform(img, b.cfg.PlatformOS, b.cfg.PlatformArch); err != nil {
		return "", err
	}

	digest, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("read digest: %w", err)
	}

	// Flatten: export the squashed image as a single tar stream (crane does
	// the layer replay + whiteout resolution for us), then extract into
	// destDir. Less code than hand-rolling per-layer replay and already
	// battle-tested by the crane CLI.
	pr, pw := io.Pipe()
	exportErr := make(chan error, 1)
	go func() {
		defer pw.Close()
		exportErr <- crane.Export(img, pw)
	}()

	if err := extractTar(ctx, pr, destDir, b.cfg.MaxUncompressedSizeBytes); err != nil {
		// Drain the export goroutine so it doesn't deadlock on pipe write.
		_ = pr.CloseWithError(err)
		<-exportErr
		return "", fmt.Errorf("extract image: %w", err)
	}
	if err := <-exportErr; err != nil {
		return "", fmt.Errorf("export image: %w", err)
	}

	return digest.String(), nil
}

// validatePlatform rejects images that don't match the required OS/arch and
// rejects known-incompatible bases (alpine, distroless) early with a clear
// message. Catches configuration mistakes before the slow mkfs.ext4 step.
func validatePlatform(img v1.Image, wantOS, wantArch string) error {
	cfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}
	if cfg.OS != wantOS {
		return fmt.Errorf("image os is %q, want %q", cfg.OS, wantOS)
	}
	if cfg.Architecture != wantArch {
		return fmt.Errorf("image architecture is %q, want %q", cfg.Architecture, wantArch)
	}
	// Heuristic: look at the config's labels and entrypoint for hints that
	// this is an alpine / distroless / busybox-only image. Cheap, catches
	// the common cases before we waste time pulling.
	for _, env := range cfg.Config.Env {
		if strings.HasPrefix(strings.ToLower(env), "path=") {
			if strings.Contains(env, "/sbin:/bin") && !strings.Contains(env, "/usr/sbin") {
				// Busybox-style layout. Alpine fits this pattern.
				return fmt.Errorf("image appears to be alpine or busybox-based (PATH looks minimal); use a debian/ubuntu-based image")
			}
		}
	}
	return nil
}

// extractTar reads a flattened image tarball and writes entries into destDir,
// preserving mode (setuid, setgid, and sticky included), symlinks, hardlinks,
// and, when running as root, ownership. Rejects path traversal (..) and
// symlinks pointing outside destDir. Enforces maxBytes total across all
// regular files when >0.
func extractTar(ctx context.Context, r io.Reader, destDir string, maxBytes int64) error {
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absDest); err != nil {
		return fmt.Errorf("stat destDir: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("destDir is not a directory: %s", absDest)
	}

	rootInfo, err := os.Stat(absDest)
	if err != nil {
		return fmt.Errorf("stat destDir: %w", err)
	}

	tr := tar.NewReader(r)
	var totalBytes int64
	// Directory modes land after their contents, so a read-only directory
	// does not block the entries extracted into it.
	var dirModes []dirMode

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Deepest first: a parent can arrive after its children (an
				// upper layer's entries come first), and once it drops owner
				// execute nothing beneath it can be changed.
				sort.SliceStable(dirModes, func(i, j int) bool {
					return strings.Count(dirModes[i].path, string(filepath.Separator)) >
						strings.Count(dirModes[j].path, string(filepath.Separator))
				})
				for _, d := range dirModes {
					if err := chmodDir(d.path, d.mode, rootInfo); err != nil {
						return err
					}
				}
				return nil
			}
			return fmt.Errorf("read tar header: %w", err)
		}
		cleaned := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("tar entry escapes destDir: %q", hdr.Name)
		}
		target := filepath.Join(absDest, cleaned)

		// Defense in depth — even after Clean/Join, make sure we stayed
		// inside the destination tree. A malicious archive could contain
		// absolute paths or symlink targets that Clean doesn't normalize.
		if rel, err := filepath.Rel(absDest, target); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("tar entry escapes destDir after join: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			// Writable while its contents are extracted; dirModes applies the
			// entry's own mode at the end.
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}

		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			// O_NOFOLLOW so an earlier symlink entry at `target` can't
			// redirect this write.
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, os.FileMode(hdr.Mode)&0o7777)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			var src io.Reader = tr
			if maxBytes > 0 {
				// One byte past the remaining budget is enough to detect the overflow.
				src = io.LimitReader(tr, maxBytes-totalBytes+1)
			}
			written, copyErr := io.Copy(f, src)
			closeErr := f.Close()
			if copyErr != nil {
				return fmt.Errorf("write %s: %w", target, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %s: %w", target, closeErr)
			}
			totalBytes += written
			if maxBytes > 0 && totalBytes > maxBytes {
				return fmt.Errorf("flattened image exceeds %d bytes", maxBytes)
			}

		case tar.TypeSymlink:
			// Rewrite absolute linknames to relative form so they resolve
			// inside the rootfs, not against the HOST root at open time.
			effectiveLinkname := hdr.Linkname
			if filepath.IsAbs(hdr.Linkname) {
				effectiveLinkname = absoluteToRelative(cleaned, hdr.Linkname)
			}
			if err := validateSymlink(absDest, target, effectiveLinkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			_ = os.Remove(target) // overwrite if exists (later layer wins)
			if err := os.Symlink(effectiveLinkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}

		case tar.TypeLink:
			linkTarget := filepath.Join(absDest, filepath.Clean(hdr.Linkname))
			if rel, err := filepath.Rel(absDest, linkTarget); err != nil || strings.HasPrefix(rel, "..") {
				return fmt.Errorf("hardlink target escapes destDir: %q → %q", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			_ = os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("hardlink %s → %s: %w", target, linkTarget, err)
			}

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Device nodes and fifos require CAP_MKNOD; we're not running
			// as root here, so skip with a warning rather than failing. The
			// guest kernel creates /dev entries at boot via devtmpfs anyway.
			continue

		default:
			// Unknown type — skip rather than abort. Matches crane's behavior.
			continue
		}

		// The archive root is the scratch directory itself. Its private host
		// mode keeps other host users away from the image's setuid files,
		// and mkfs sets the filesystem root's owner and mode regardless.
		if target == absDest {
			continue
		}
		// A hardlink shares its target's inode, which already has its owner
		// and mode.
		if hdr.Typeflag != tar.TypeLink {
			if err := restoreOwner(target, hdr); err != nil {
				return err
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			dirModes = append(dirModes, dirMode{path: target, mode: tarEntryMode(hdr)})
		case tar.TypeReg, tar.TypeRegA:
			// After the owner: chown clears setuid and setgid.
			if err := os.Chmod(target, tarEntryMode(hdr)); err != nil {
				return fmt.Errorf("chmod %s: %w", target, err)
			}
		}

		// Preserve mtime for files/symlinks where possible. Best-effort;
		// ignore errors (not all filesystems support sub-second precision).
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
		}
	}
}

type dirMode struct {
	path string
	mode os.FileMode
}

// chmodDir sets a directory entry's mode without following a symlink in its
// final component, and never on the extraction root however the path reaches
// it: an image can make an entry's path an alias of the root (alias -> .),
// and the root's private mode is what keeps host users from its setuid files.
func chmodDir(path string, mode os.FileMode, root os.FileInfo) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		// A later entry replaced the directory; its header no longer applies.
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if os.SameFile(info, root) {
		return nil
	}
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

// restoreOwner gives an entry the owner its layer records, as a container
// runtime does; a file owned by a service account must not become root's.
// Only root can do this, so without it entries stay owned by the builder.
func restoreOwner(target string, hdr *tar.Header) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Lchown(target, hdr.Uid, hdr.Gid); err != nil {
		return fmt.Errorf("chown %s: %w", target, err)
	}
	return nil
}

// tarEntryMode returns an entry's permission bits with setuid, setgid, and
// sticky, which os.FileMode keeps as flags rather than as the 0o7000 bits a
// raw conversion of the header's mode would carry.
func tarEntryMode(hdr *tar.Header) os.FileMode {
	return hdr.FileInfo().Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

// validateSymlink rejects symlinks whose target escapes absDest. Callers
// must rewrite absolute linknames to relative form before calling this.
func validateSymlink(absDest, linkPath, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("internal: absolute linkname reached validateSymlink: %s → %s", linkPath, linkname)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), linkname))
	if rel, err := filepath.Rel(absDest, resolved); err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("symlink target escapes rootfs: %s → %s", linkPath, linkname)
	}
	return nil
}

// absoluteToRelative converts a rootfs-absolute linkname into a path
// relative to the symlink's own parent directory.
func absoluteToRelative(cleaned, linkname string) string {
	linkDirAbs := filepath.Join("/", filepath.Dir(cleaned))
	targetAbs := filepath.Clean(linkname)
	rel, err := filepath.Rel(linkDirAbs, targetAbs)
	if err != nil {
		return targetAbs
	}
	return rel
}

// umaskZero sets the process umask to 0 for the duration of tar extraction
// so file permissions from the tar entries land verbatim (no 022 masking).
// Returns a restore function; callers defer it.
func umaskZero() func() {
	old := syscall.Umask(0)
	return func() { syscall.Umask(old) }
}
