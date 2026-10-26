package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ClaimResumeWithCurrentLimits holds the assignment mutex while reading shape
// caps and claiming compute. SHARE permits parallel resumes but blocks cap
// updates and plan assignment's NO KEY UPDATE lock. The transaction ends before
// the caller performs boot I/O or waits for another claim attempt.
func (q *Queries) ClaimResumeWithCurrentLimits(ctx context.Context, arg ClaimResumeParams) (ClaimResumeRow, GetTeamEffectiveLimitsRow, error) {
	var row ClaimResumeRow
	var limits GetTeamEffectiveLimitsRow
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
	locked, err := tx.Exec(ctx, `SELECT 1 FROM team WHERE id=$1 FOR SHARE`, arg.TeamID)
	if err != nil {
		return row, limits, err
	}
	if locked.RowsAffected() != 1 {
		return row, limits, fmt.Errorf("resume team does not exist")
	}
	qt := q.WithTx(tx)
	limits, err = qt.GetTeamEffectiveLimits(ctx, arg.TeamID)
	if err != nil {
		// A missing limits row must not enter the sandbox-state retry path.
		return row, limits, fmt.Errorf("read resume plan limits: %v", err)
	}
	if limits.MaxVcpu <= 0 || limits.MaxMemoryMib <= 0 || limits.MaxDiskMib <= 0 {
		return row, limits, fmt.Errorf("invalid resume plan limits")
	}
	arg.MaxVcpu, arg.MaxMemoryMib, arg.MaxDiskMib = &limits.MaxVcpu, &limits.MaxMemoryMib, &limits.MaxDiskMib
	row, err = qt.ClaimResume(ctx, arg)
	if err != nil {
		return row, limits, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ClaimResumeRow{}, limits, err
	}
	return row, limits, nil
}
