package vm

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Every refusal happens before the revival teardown, which removes the
// paused disk: a shape Firecracker would reject at boot must not cost the
// sandbox its pause.
func TestReshapeVMRefusesBeforeTeardown(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager()
	m.cfg = ManagerConfig{RunDir: filepath.Join(dir, "run"), SnapshotDir: filepath.Join(dir, "snapshots")}
	m.vms = map[string]*VMInstance{
		"vm-running": {ID: "vm-running", Status: StatusRunning, Config: VMConfig{VCPU: 1, MemoryMiB: 1024}},
		"vm-gone":    {ID: "vm-gone", Status: StatusPaused, SnapshotPath: filepath.Join(dir, "snapshots", "vm-gone", "vmstate.snap"), Config: VMConfig{VCPU: 1, MemoryMiB: 1024}},
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name      string
		id        string
		vcpu, mem uint32
		want      codes.Code
	}{
		{"traversal id", "../escape", 2, 2048, codes.InvalidArgument},
		{"zero vcpu", "vm-running", 0, 2048, codes.InvalidArgument},
		{"too many vcpus", "vm-running", reshapeMaxVCPU + 1, 2048, codes.InvalidArgument},
		{"too little memory", "vm-running", 2, reshapeMinMemMiB - 1, codes.InvalidArgument},
		{"running at another shape", "vm-running", 2, 2048, codes.FailedPrecondition},
		{"paused with nothing to boot", "vm-gone", 2, 2048, codes.FailedPrecondition},
	} {
		if _, err := m.ReshapeVM(ctx, tc.id, tc.vcpu, tc.mem, nil); status.Code(err) != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}

	// A retry after a lost response finds the VM already at its target.
	inst, err := m.ReshapeVM(ctx, "vm-running", 1, 1024, nil)
	if err != nil || inst.ID != "vm-running" {
		t.Fatalf("retry at the current shape: %v, want the running VM", err)
	}
}
