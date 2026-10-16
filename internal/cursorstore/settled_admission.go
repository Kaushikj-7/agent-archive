package cursorstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// ReadSettledAdmission reads composer content and borrowed catalog metadata on
// one settled immutable transaction. It confers no durable publication authority.
// The callback must not retain metadata rows; the caller owns composer charges.
func ReadSettledAdmission(ctx context.Context, path, id string, rawLimit, recordLimit int64, budget agentapi.RecoveryReadBudget, visit func(agentapi.DatabaseRecord) error) (c Composer, err error) {
	if id == "" || visit == nil {
		return c, NotChecked(UnknownFormat)
	}
	err = ReadRecovery(ctx, path, budget, func(ctx context.Context, db *sql.DB) (err error) {
		c, err = readAdmissionComposer(ctx, db, id, rawLimit, recordLimit, budget, visit)
		return err
	})
	if err != nil {
		return Composer{}, err
	}
	return c, nil
}

func visitSettledComposerMetadata(ctx context.Context, tx *sql.Tx, budget agentapi.RecoveryReadBudget, visit func(agentapi.DatabaseRecord) error) (err error) {
	const lower = "composerData:"
	const upper = "composerData;"
	if budget.RemainingRows() <= 0 {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	count, err := countSettledComposerMetadata(ctx, tx, budget, lower, upper)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key LIMIT ?`, lower, upper, count+1)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	seen := 0
	for rows.Next() {
		if seen >= count {
			return NotChecked(ChangedDuringRead)
		}
		var record agentapi.DatabaseRecord
		if err = rows.Scan(&record.Key, &record.Value); err != nil {
			return err
		}
		if err = visit(record); err != nil {
			return err
		}
		seen++
	}
	if seen != count {
		return NotChecked(ChangedDuringRead)
	}
	return errors.Join(rows.Err(), ctx.Err())
}

func countSettledComposerMetadata(ctx context.Context, tx *sql.Tx, budget agentapi.RecoveryReadBudget, lower, upper string) (count int, err error) {
	lengths, err := tx.QueryContext(ctx, `SELECT length(CAST(key AS BLOB)),length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key LIMIT ?`, lower, upper, budget.RemainingRows()+1)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, lengths.Close()) }()
	for {
		if err = budget.Charge(1, 16); err != nil {
			return 0, err
		}
		if !lengths.Next() {
			break
		}
		var keyBytes, valueBytes sql.NullInt64
		if err = lengths.Scan(&keyBytes, &valueBytes); err != nil {
			return 0, err
		}
		if !keyBytes.Valid || keyBytes.Int64 < 0 || !valueBytes.Valid || valueBytes.Int64 < 0 || keyBytes.Int64 > budget.RemainingBytes() || valueBytes.Int64 > budget.RemainingBytes()-keyBytes.Int64 {
			return 0, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		// Reserve key, value and row metadata before the payload query allocates.
		if err = budget.Charge(1, keyBytes.Int64+valueBytes.Int64); err != nil {
			return 0, err
		}
		count++
	}
	return count, lengths.Err()
}

func readAdmissionComposer(ctx context.Context, db *sql.DB, id string, rawLimit, recordLimit int64, budget agentapi.RecoveryReadBudget, visit func(agentapi.DatabaseRecord) error) (c Composer, err error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return c, err
	}
	defer func() { err = errors.Join(err, tx.Rollback()) }()
	c, err = readRecoveryComposerTx(ctx, tx, id, rawLimit, recordLimit, budget, true)
	if err != nil {
		return Composer{}, err
	}
	err = visitSettledComposerMetadata(ctx, tx, budget, visit)
	return c, err
}
