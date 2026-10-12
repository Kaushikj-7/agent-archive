package cursorstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// ReadRecovery reads settled in-place evidence without preparing a snapshot.
// Header observations and all query payloads share the caller's epoch budget.
// Live WAL sources require a fresh settled observation and are never copied:
// one whose -wal holds frames is Locked, and one whose -wal is empty (with or
// without -shm) is read immutably and checked afterwards (see settled).
func ReadRecovery(ctx context.Context, path string, budget agentapi.RecoveryReadBudget, read func(context.Context, *sql.DB) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget == nil {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	// resolve and immutable post-read verification each read a 100-byte header.
	if err := budget.Charge(0, 200); err != nil {
		return err
	}
	src, err := resolve(path)
	if err != nil {
		return err
	}
	if src.live {
		settled, ok := src.settled()
		if !ok {
			// Frames in the -wal are only safely read through Cursor's
			// shared-memory index, which a settled observation can't vouch
			// for: immutable would ignore them.
			return NotChecked(Locked)
		}
		// settled and the post-read check each read the -shm header.
		if err := budget.Charge(0, 2*shmHeaderSize); err != nil {
			return err
		}
		src = settled
	}
	return readInPlace(ctx, src, Options{}, func(ctx context.Context, db *sql.DB) error {
		// Native decoding and payload preflights require one row per exact key.
		// Explicit collations are outside the known native schema: the decoder's
		// ordinary selectors must retain exact binary key comparisons.
		// Keep this scalar schema observation on the same immutable handle.
		if err := budget.Charge(1, 8); err != nil {
			return err
		}
		var unique bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM pragma_index_list('cursorDiskKV') AS idx
 WHERE idx."unique" = 1 AND idx.partial = 0
 AND EXISTS (SELECT 1 FROM sqlite_schema
 WHERE type = 'table' AND name = 'cursorDiskKV' AND sql NOT LIKE '%COLLATE%')
 AND (SELECT count(*) FROM pragma_index_xinfo(idx.name) WHERE "key" = 1) = 1
 AND EXISTS (SELECT 1 FROM pragma_index_xinfo(idx.name)
 WHERE "key" = 1 AND name = 'key' AND coll = 'BINARY')
)`).Scan(&unique)
		if err != nil {
			return err
		}
		if !unique {
			return NotChecked(UnknownFormat)
		}
		return read(ctx, db)
	})
}

// ReadRecoveryComposer returns bounded immutable evidence with the existing
// native relationship decoder, without live signatures or failure signatures.
func ReadRecoveryComposer(ctx context.Context, path, id string, rawLimit, recordLimit int64, budget agentapi.RecoveryReadBudget) (c Composer, err error) {
	if id == "" {
		return c, ErrComposerNotFound
	}
	err = ReadRecovery(ctx, path, budget, func(ctx context.Context, db *sql.DB) (err error) {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, tx.Rollback()) }()
		value, err := recoveryComposerValue(ctx, tx, id, rawLimit, recordLimit, budget)
		if err != nil {
			return err
		}
		_, ids, err := decodeHeaders(value)
		if err != nil {
			return err
		}
		lengths, err := recoveryBubbleLengths(ctx, tx, id, budget)
		if err != nil {
			return err
		}
		total := int64(len(value))
		seen := map[string]bool{}
		prefix := bubblePrefix(id)
		for _, key := range ids {
			n, present := lengths[key]
			if !present {
				continue
			}
			if rawLimit > 0 && n > rawLimit-total {
				return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
			}
			total += n
			if seen[key] {
				continue
			}
			seen[key] = true
			if recordLimit > 0 && n > recordLimit {
				return agentapi.Wrap(agentapi.Limit, ErrRecordLimit)
			}
			if err := budget.Charge(1, int64(len(prefix+key))+n); err != nil {
				return err
			}
		}
		found, err := listedBubbles(ctx, tx, id, ids, lengths)
		if err != nil {
			return err
		}
		c = Composer{Composer: value, Bubbles: make([]Bubble, len(ids))}
		for i, key := range ids {
			_, present := lengths[key]
			c.Bubbles[i] = Bubble{ID: key, Value: found[key], Missing: !present}
		}
		return nil
	})
	return c, err
}

func recoveryComposerValue(ctx context.Context, tx *sql.Tx, id string, rawLimit, recordLimit int64, budget agentapi.RecoveryReadBudget) ([]byte, error) {
	if err := budget.Charge(1, 8); err != nil {
		return nil, err
	}
	var size sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key = ?`, "composerData:"+id).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !size.Valid {
		return nil, ErrComposerNotFound
	}
	if err != nil {
		return nil, err
	}
	if rawLimit > 0 && size.Int64 > rawLimit {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	if recordLimit > 0 && size.Int64 > recordLimit {
		return nil, agentapi.Wrap(agentapi.Limit, ErrRecordLimit)
	}
	if err := budget.Charge(1, size.Int64); err != nil {
		return nil, err
	}
	return composerRow(ctx, tx, id)
}

func recoveryBubbleLengths(ctx context.Context, tx *sql.Tx, id string, budget agentapi.RecoveryReadBudget) (out map[string]int64, err error) {
	if budget.RemainingRows() <= 0 || budget.RemainingBytes() < 16 {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	prefix := bubblePrefix(id)
	// Charge numeric length projections before reading or allocating native keys.
	rows, err := tx.QueryContext(ctx, `SELECT length(CAST(key AS BLOB)), length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= ? AND key < ? LIMIT ?`, prefix, bubbleUpper(prefix), budget.RemainingRows())
	if err != nil {
		return nil, err
	}
	remainingRows := budget.RemainingRows()
	count, total := 0, int64(0)
	for {
		// Even fixed-size SQL metadata must fit before the driver's next read.
		if budget.RemainingRows() <= 0 || budget.RemainingBytes() < 16 {
			return nil, errors.Join(agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit), rows.Close())
		}
		if !rows.Next() {
			break
		}
		if err := budget.Charge(1, 16); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		var key, size sql.NullInt64
		if err := rows.Scan(&key, &size); err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		count++
		total += key.Int64 + 8
		if count > remainingRows || total > budget.RemainingBytes() {
			return nil, errors.Join(agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit), rows.Close())
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if err := budget.Charge(count, total); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT key,length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= ? AND key < ? LIMIT ?`, prefix, bubbleUpper(prefix), count)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out = map[string]int64{}
	for rows.Next() {
		var key string
		var size sql.NullInt64
		if err := rows.Scan(&key, &size); err != nil {
			return nil, err
		}
		out[key[len(prefix):]] = size.Int64
	}
	return out, rows.Err()
}
