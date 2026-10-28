package builder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
)

// Builder produces a Firecracker-bootable rootfs.ext4 from a BuildSpec.
// The default implementation is in-process: pulls OCI via
// go-containerregistry, injects boxd as an additional layer, and runs
// mkfs.ext4 locally. The interface lets callers swap implementations.
type Builder interface {
	// BuildRootfs resolves the base image, flattens layers, injects boxd,
	// and writes rootfs.ext4 at destPath. Returns the resolved digest and
	// the on-disk size of the result.
	//
	// This does NOT execute user build steps (RUN/COPY/ENV/WORKDIR) — those
	// run later inside a Firecracker VM booted from the produced rootfs.
	BuildRootfs(ctx context.Context, spec BuildSpec, destPath string, sizeMiB uint32) (BuildRootfsResult, error)
}

// NewBuilder returns the default in-process Builder implementation.
func NewBuilder(cfg Config) (Builder, error) {
	cfg.ApplyDefaults()
	if cfg.BoxdBinaryPath == "" {
		return nil, fmt.Errorf("builder.Config.BoxdBinaryPath is required")
	}
	return &inProcessBuilder{cfg: cfg}, nil
}

// inProcessBuilder is the only implementation today. Stateless; safe to
// construct once and reuse across builds.
type inProcessBuilder struct {
	cfg Config
}

// BuildRootfs runs the pull → inject → mkfs pipeline in order and returns
// the resolved digest + on-disk size. Uses a scratch directory that is
// cleaned up on both success and failure.
func (b *inProcessBuilder) BuildRootfs(ctx context.Context, spec BuildSpec, destPath string, sizeMiB uint32) (BuildRootfsResult, error) {
	if spec.From == "" {
		return BuildRootfsResult{}, fmt.Errorf("spec.from is required")
	}
	if destPath == "" {
		return BuildRootfsResult{}, fmt.Errorf("destPath is required")
	}
	if sizeMiB == 0 {
		return BuildRootfsResult{}, fmt.Errorf("sizeMiB must be > 0")
	}

	logger := log.With().Str("component", "builder").Str("from", spec.From).Logger()

	// Scratch dir lives under the destPath's parent so we never cross
	// filesystems mid-build (matters for hardlink preservation during tar
	// extraction, and is faster than a cross-device copy).
	scratch, err := os.MkdirTemp(destPath+".scratch", "rootfs-*")
	if err != nil {
		// MkdirTemp wants an existing parent; fall back to the destPath's
		// directory if the .scratch sibling doesn't exist yet.
		scratch, err = os.MkdirTemp("", "rootfs-*")
		if err != nil {
			return BuildRootfsResult{}, fmt.Errorf("create scratch dir: %w", err)
		}
	}
	defer func() {
		if rmErr := os.RemoveAll(scratch); rmErr != nil {
			logger.Warn().Err(rmErr).Str("dir", scratch).Msg("failed to clean up scratch dir")
		}
	}()

	// Umask=0 during extraction so file modes from the OCI layers land
	// verbatim. Restore on exit.
	defer umaskZero()()

	logger.Info().Str("scratch", scratch).Msg("pulling + flattening OCI image")
	digest, image, err := b.pullAndFlatten(ctx, spec.From, scratch)
	if err != nil {
		return BuildRootfsResult{}, fmt.Errorf("pull %s: %w", spec.From, err)
	}
	logger.Info().Str("digest", digest).Msg("image flattened")

	if err := requireShell(scratch); err != nil {
		return BuildRootfsResult{}, err
	}

	boxdBytes, err := injectGuestAgent(ctx, scratch, b.cfg.BoxdBinaryPath, b.cfg.FreezeWorkload, &logger)
	if err != nil {
		return BuildRootfsResult{}, fmt.Errorf("inject boxd: %w", err)
	}
	logger.Info().Int64("boxd_bytes", boxdBytes).Msg("boxd injected")

	if err := injectProxyCA(scratch, b.cfg.ProxyCACertPath, &logger); err != nil {
		return BuildRootfsResult{}, fmt.Errorf("inject proxy CA: %w", err)
	}

	if err := makeExt4(ctx, scratch, destPath, sizeMiB); err != nil {
		return BuildRootfsResult{}, fmt.Errorf("make ext4: %w", err)
	}

	info, err := os.Stat(destPath)
	if err != nil {
		return BuildRootfsResult{}, fmt.Errorf("stat produced rootfs: %w", err)
	}
	logger.Info().Str("path", destPath).Int64("size_bytes", info.Size()).Msg("rootfs.ext4 ready")

	return BuildRootfsResult{
		RootfsPath:     destPath,
		ResolvedDigest: digest,
		SizeBytes:      info.Size(),
		Image:          image,
	}, nil
}

// errNoShell marks a base image without /bin/sh, which the guest init and
// every build step run under.
var errNoShell = errors.New("image has no /bin/sh")

// guestTools are the commands every boot and build runs whatever the build
// spec says: the injected init creates and mounts its filesystems and its
// freezer cgroup, and each build syncs the disk before the snapshot.
var guestTools = []string{"mkdir", "mount", "chmod", "sync"}

// guestPath is where those commands are looked up; the injected init exports
// the same list as its PATH.
var guestPath = []string{"usr/local/sbin", "usr/local/bin", "usr/sbin", "usr/bin", "sbin", "bin"}

func guestPathEnv() string {
	dirs := make([]string, len(guestPath))
	for i, dir := range guestPath {
		dirs[i] = "/" + dir
	}
	return strings.Join(dirs, ":")
}

// requireShell rejects a flattened image that cannot boot or build: one
// whose /bin/sh is not an executable file, such as a distroless or
// scratch-based image, or one missing a command in guestTools.
func requireShell(rootfsDir string) error {
	root, err := filepath.EvalSymlinks(rootfsDir)
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}
	if !isExecutable(root, "bin/sh") {
		return errNoShell
	}
	var missing []string
	for _, tool := range guestTools {
		found := false
		for _, dir := range guestPath {
			if isExecutable(root, filepath.Join(dir, tool)) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("image is missing required commands: %s", strings.Join(missing, ", "))
	}
	return nil
}

// isExecutable reports whether rel names an executable regular file inside
// root. Extraction keeps every symlink relative and inside the rootfs, so
// resolving one here cannot leave it.
func isExecutable(root, rel string) bool {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return false
	}
	if r, err := filepath.Rel(root, resolved); err != nil || strings.HasPrefix(r, "..") {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
