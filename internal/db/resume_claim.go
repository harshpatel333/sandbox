package db

import (
	"context"
	"fmt"
	"time"

	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ResumeShapeLimits preserves absent Free dimensions. Other plans must resolve
// all dimensions to positive caps before a claim can run.
type ResumeShapeLimits struct {
	Free                              bool
	MaxVcpu, MaxMemoryMib, MaxDiskMib *int32
}

func (l ResumeShapeLimits) Exceeds(cpu, memory, disk int32) bool {
	return l.MaxVcpu != nil && cpu > *l.MaxVcpu || l.MaxMemoryMib != nil && memory > *l.MaxMemoryMib || l.MaxDiskMib != nil && disk > *l.MaxDiskMib
}

// ClaimResumeWithCurrentLimits holds the assignment mutex while reading shape
// caps and claiming compute. SHARE permits parallel resumes but blocks cap
// updates and plan assignment's NO KEY UPDATE lock. The transaction ends before
// the caller performs boot I/O or waits for another claim attempt.
func (q *Queries) ClaimResumeWithCurrentLimits(ctx context.Context, arg ClaimResumeParams) (ClaimResumeRow, ResumeShapeLimits, error) {
	var row ClaimResumeRow
	var limits ResumeShapeLimits
	beginner, ok := q.db.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	})
	if !ok {
		return row, limits, fmt.Errorf("resume claim requires a transaction-capable connection")
	}
	tx, err := beginner.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return row, limits, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	locked, err := tx.Exec(ctx, `SELECT 1 FROM team WHERE id=$1 FOR SHARE NOWAIT`, arg.TeamID)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "55P03" {
			return row, limits, &pgconn.PgError{Code: "SS003", Message: "team plan assignment is changing"}
		}
		return row, limits, err
	}
	if locked.RowsAffected() != 1 {
		return row, limits, fmt.Errorf("resume team does not exist")
	}
	qt := q.WithTx(tx)
	err = tx.QueryRow(ctx, `-- resume shape limits
SELECT f.free_since IS NOT NULL,
 CASE WHEN f.free_since IS NOT NULL THEN f.max_vcpu ELSE l.max_vcpu END,
 CASE WHEN f.free_since IS NOT NULL THEN f.max_memory_mib ELSE l.max_memory_mib END,
 CASE WHEN f.free_since IS NOT NULL THEN f.max_disk_mib ELSE l.max_disk_mib END
FROM team_effective_limits($1) l LEFT JOIN billing_free_plan($1) f ON true`, arg.TeamID).Scan(&limits.Free, &limits.MaxVcpu, &limits.MaxMemoryMib, &limits.MaxDiskMib)
	if err != nil {
		// A missing limits row must not enter the sandbox-state retry path.
		return row, limits, fmt.Errorf("read resume plan limits: %v", err)
	}
	for _, cap := range []*int32{limits.MaxVcpu, limits.MaxMemoryMib, limits.MaxDiskMib} {
		if cap != nil && *cap <= 0 || cap == nil && !limits.Free {
			return row, limits, fmt.Errorf("invalid resume plan limits")
		}
	}
	arg.MaxVcpu, arg.MaxMemoryMib, arg.MaxDiskMib = limits.MaxVcpu, limits.MaxMemoryMib, limits.MaxDiskMib
	row, err = qt.ClaimResume(ctx, arg)
	if err != nil {
		return row, limits, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ClaimResumeRow{}, limits, err
	}
	return row, limits, nil
}
