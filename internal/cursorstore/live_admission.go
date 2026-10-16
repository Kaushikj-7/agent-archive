package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// ReadLiveAdmission decodes fixed composer and catalog observations from the
// same held bounded image produced by the pinned source transaction. Neither
// the workspace nor this IO operation confers native capture proof authority.
// The caller retains the original resource scope through all returned views.
func ReadLiveAdmission(ctx context.Context, path, id string, rawLimit, recordLimit int64, budget agentapi.RecoveryReadBudget, workspace LiveAdmissionWorkspace, visit func(agentapi.DatabaseRecord) error) (c Composer, err error) {
	if budget == nil || workspace == nil || id == "" || visit == nil {
		return c, NotChecked(UnknownFormat)
	}
	// Bounded SQLite caches, WAL-index mappings and page buffers coexist with the
	// returned logical views; charge their allowance before opening the source.
	if err = budget.Charge(0, 32<<20); err != nil {
		return c, err
	}
	_, _, err = prepareAdmissionDatabase(ctx, path, workspace, admissionCopyOptions{afterImage: func(held *os.File, imagePath string) error {
		return readHeldAdmissionImage(ctx, held, imagePath, budget, func(ctx context.Context, db *sql.DB) (err error) {
			c, err = readAdmissionComposer(ctx, db, id, rawLimit, recordLimit, budget, visit)
			return err
		})
	}})
	if err != nil {
		return Composer{}, err
	}
	return c, nil
}

func readHeldAdmissionImage(ctx context.Context, held *os.File, path string, budget agentapi.RecoveryReadBudget, read func(context.Context, *sql.DB) error) (err error) {
	before, err := held.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return errors.Join(NotChecked(Unreadable), err)
	}
	vfs, err := newAdmissionDestinationVFS(ctx, path, held, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, vfs.Close()) }()
	db, err := sql.Open("sqlite", admissionDSN(dsn(path, false), vfs.name))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	conn, err := admissionConnection(ctx, db)
	if err != nil {
		return err
	}
	if err = conn.Close(); err != nil {
		return err
	}
	if err = validateRecoverySchema(ctx, db, budget); err != nil {
		return err
	}
	err = read(ctx, db)
	after, statErr := held.Stat()
	if statErr != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.Join(err, NotChecked(ChangedDuringRead), statErr)
	}
	return errors.Join(err, ctx.Err())
}
