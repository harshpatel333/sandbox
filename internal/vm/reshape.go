package vm

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Firecracker refuses more vCPUs than this; checked before the teardown,
// which a boot-time refusal would otherwise follow.
const (
	reshapeMaxVCPU   = 32
	reshapeMinMemMiB = 128
)

// ReshapeVM cold-boots a paused sandbox from its own disk at a new vCPU
// count and memory size. A snapshot only restores at the shape it was
// taken, so memory and processes are lost and files survive.
//
// The disk and block map are staged outside the run directory first,
// because the revival teardown removes it. A failed boot leaves the
// sandbox paused with its artifacts gone and the staged copy in place;
// a retry boots from that copy.
func (m *Manager) ReshapeVM(ctx context.Context, vmID string, vcpu, memMiB uint32, rules *sandboxNetworkRules) (*VMInstance, error) {
	if !isLeafName(vmID) || isReservedRunDirName(vmID) {
		return nil, status.Error(codes.InvalidArgument, "vm_id must be a valid per-VM identifier")
	}
	if vcpu == 0 || vcpu > reshapeMaxVCPU {
		return nil, status.Errorf(codes.InvalidArgument, "vcpu must be between 1 and %d", reshapeMaxVCPU)
	}
	if memMiB < reshapeMinMemMiB {
		return nil, status.Errorf(codes.InvalidArgument, "mem_mib must be at least %d", reshapeMinMemMiB)
	}
	unlock, err := m.lockVMOp(ctx, vmID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	inst, err := m.getInstance(vmID)
	if err != nil {
		return nil, err
	}
	inst.mu.RLock()
	st, unverified := inst.Status, inst.Unverified
	curVCPU, curMem := inst.Config.VCPU, inst.Config.MemoryMiB
	snap, mem, baseMem, base := inst.SnapshotPath, inst.MemFilePath, inst.BaseMemPath, inst.Config.BasePath
	teamID, ownerID := inst.TeamID, inst.OwnerID
	inst.mu.RUnlock()

	// A retry after a lost response finds its own work done.
	if st == StatusRunning && !unverified && curVCPU == vcpu && curMem == memMiB {
		return inst, nil
	}
	if st != StatusPaused || snap == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "vm %s must be paused to reshape", vmID)
	}

	stageDir := filepath.Join(m.cfg.SnapshotDir, vmID, "reshape")
	diskName := "rootfs.ext4"
	if base != "" {
		diskName = "overlay.ext4"
	}
	stagedDisk := filepath.Join(stageDir, diskName)
	stagedMap := ""

	disk := m.instanceDiskPath(inst)
	if pauseArtifactsPresent(snap, mem, baseMem, disk, base) {
		// The paused disk is authoritative; any copy left by an earlier
		// failed attempt predates it and is replaced.
		if err := os.RemoveAll(stageDir); err != nil {
			return nil, status.Errorf(codes.Internal, "clear reshape staging: %v", err)
		}
		if err := os.MkdirAll(stageDir, 0o700); err != nil {
			return nil, status.Errorf(codes.Internal, "reshape staging: %v", err)
		}
		if err := stageReshapeFile(ctx, disk, stagedDisk); err != nil {
			return nil, status.Errorf(codes.Internal, "stage disk: %v", err)
		}
		if blockMap := overlayBlockMapPath(snap); base != "" && statRegularFile(blockMap) {
			stagedMap = filepath.Join(stageDir, filepath.Base(blockMap))
			if err := stageReshapeFile(ctx, blockMap, stagedMap); err != nil {
				return nil, status.Errorf(codes.Internal, "stage block map: %v", err)
			}
		}
	} else if statRegularFile(stagedDisk) {
		if p := filepath.Join(stageDir, filepath.Base(overlayBlockMapPath(snap))); base != "" && statRegularFile(p) {
			stagedMap = p
		}
	} else {
		return nil, status.Errorf(codes.FailedPrecondition, "vm %s has neither its pause artifacts nor a staged reshape disk", vmID)
	}

	start := time.Now()
	revived, err := m.reviveVMLocked(ctx, vmID, stagedDisk, base, stagedMap, false, false, true, teamID, ownerID, vcpu, memMiB, rules, "")
	if err != nil {
		return nil, err
	}
	_ = os.RemoveAll(stageDir)
	m.log.Info().Str("vm_id", vmID).
		Uint32("from_vcpu", curVCPU).Uint32("from_mem_mib", curMem).
		Uint32("vcpu", vcpu).Uint32("mem_mib", memMiB).
		Dur("total", time.Since(start)).
		Msg("sandbox reshaped")
	return revived, nil
}

// stageReshapeFile reflinks src to dst through a temporary name, so a
// crash mid-copy never leaves a partial file under the name a retry boots.
func stageReshapeFile(ctx context.Context, src, dst string) error {
	tmp := dst + ".tmp"
	if err := reflinkFileExact(ctx, src, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
