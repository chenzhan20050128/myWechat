package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	apperrors "github.com/example/wechat/internal/platform/errors"
	"github.com/example/wechat/internal/platform/mysqlx"
)

// Sources is the consumer-defined port over other domains.
type Sources interface {
	Profile(ctx context.Context, userID int64) (json.RawMessage, error)
	ContactSettings(ctx context.Context, userID int64) (json.RawMessage, error)
	Favorites(ctx context.Context, userID int64) (json.RawMessage, error)
	OwnMoments(ctx context.Context, userID int64) (json.RawMessage, error)
	ReadableMessages(ctx context.Context, userID int64) (json.RawMessage, error)
}

// Service owns backup lifecycle.
type Service struct {
	db      *sql.DB
	sources Sources
	now     func() time.Time
}

func New(db *sql.DB, s Sources, now func() time.Time) *Service {
	return &Service{db: db, sources: s, now: now}
}

// RequestResult is E1 response.
type RequestResult struct {
	BackupID int64 `json:"backup_id,string"`
}

// Request creates a backup row (status=creating) and synchronously runs the
// export. V1 skips the worker queue; the HTTP call blocks until ready or failed
// (the sources port is small in dev).
func (s *Service) Request(ctx context.Context, accountID int64) (*BackupRow, error) {
	scope := json.RawMessage(`{"sections":["profile","contact_settings","favorites","moments","messages"]}`)
	now := s.now()
	var backupID int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if err := upsertSlot(ctx, tx, accountID, now); err != nil {
			return err
		}
		if _, err := lockSlot(ctx, tx, accountID); err != nil {
			return err
		}
		creating, err := activeBackupCreating(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if creating {
			return apperrors.New(apperrors.StateConflict, "backup already in progress")
		}
		id, err := insertBackup(ctx, tx, accountID, scope, now)
		if err != nil {
			return err
		}
		backupID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Synchronous export: pull sections, assemble size, flip to ready.
	size, err := s.exportSectionsImpl(ctx, accountID, backupID)
	if err != nil {
		_ = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
			return failBackup(ctx, tx, backupID, err.Error(), now)
		})
		return nil, err
	}
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return completeBackup(ctx, tx, backupID, accountID, size, now)
	})
	if err != nil {
		return nil, err
	}
	return findBackup(ctx, s.db, backupID, accountID)
}

// exportSectionsImpl pulls each section from the Sources port and returns the
// total payload size. V1 writes only the byte count (object-store shunting is
// deferred to the runtime module; the row's total_size tracks snapshot size).
func (s *Service) exportSectionsImpl(ctx context.Context, accountID, backupID int64) (int64, error) {
	sections := map[string]json.RawMessage{}
	p, err := s.sources.Profile(ctx, accountID)
	if err != nil {
		return 0, err
	}
	sections["profile"] = p
	c, err := s.sources.ContactSettings(ctx, accountID)
	if err != nil {
		return 0, err
	}
	sections["contact_settings"] = c
	f, err := s.sources.Favorites(ctx, accountID)
	if err != nil {
		return 0, err
	}
	sections["favorites"] = f
	m, err := s.sources.OwnMoments(ctx, accountID)
	if err != nil {
		return 0, err
	}
	sections["moments"] = m
	msg, err := s.sources.ReadableMessages(ctx, accountID)
	if err != nil {
		return 0, err
	}
	sections["messages"] = msg
	var total int64
	for _, p := range sections {
		total += int64(len(p))
	}
	return total, nil
}

// Current returns the active backup slot.
func (s *Service) Current(ctx context.Context, accountID int64) (*BackupRow, error) {
	b, err := findActiveBackup(ctx, s.db, accountID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return &BackupRow{AccountID: accountID, Status: "none"}, nil
	}
	return b, nil
}

// DeleteCurrent removes the active backup.
func (s *Service) DeleteCurrent(ctx context.Context, accountID int64) error {
	b, err := findActiveBackup(ctx, s.db, accountID)
	if err != nil {
		return err
	}
	if b == nil {
		return apperrors.Unavail("no active backup")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return deleteBackup(ctx, tx, b.ID, accountID, now)
	})
}

// Restore runs the six-step restore flow.
func (s *Service) Restore(ctx context.Context, accountID int64) (int64, error) {
	b, err := findActiveBackup(ctx, s.db, accountID)
	if err != nil {
		return 0, err
	}
	if b == nil || b.Status != "ready" {
		return 0, apperrors.New(apperrors.StateConflict, "no ready backup")
	}
	now := s.now()
	var jobID int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		if err := setBackupRestoring(ctx, tx, b.ID); err != nil {
			return err
		}
		id, err := insertRestoreJob(ctx, tx, accountID, b.ID, now)
		if err != nil {
			return err
		}
		jobID = id
		// Stage each section.
		sections := map[string]json.RawMessage{}
		p, err := s.sources.Profile(ctx, accountID)
		if err != nil {
			return err
		}
		sections["profile"] = p
		for sec, payload := range sections {
			if err := upsertStaging(ctx, tx, jobID, sec, payload, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
			_ = setBackupReady(ctx, tx, b.ID)
			return failRestoreJob(ctx, tx, jobID, err.Error(), now)
		})
		return 0, err
	}
	// Promote staging -> archive.
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		staging, err := listStaging(ctx, tx, jobID)
		if err != nil {
			return err
		}
		profile := staging["profile"]
		size := int64(len(profile))
		if _, err := insertProfileSnapshot(ctx, tx, accountID, jobID, profile, size, now); err != nil {
			return err
		}
		if err := deleteStaging(ctx, tx, jobID); err != nil {
			return err
		}
		if err := completeRestoreJob(ctx, tx, jobID, now); err != nil {
			return err
		}
		return setBackupReady(ctx, tx, b.ID)
	})
	if err != nil {
		_ = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
			_ = setBackupReady(ctx, tx, b.ID)
			return failRestoreJob(ctx, tx, jobID, err.Error(), now)
		})
		return 0, err
	}
	return jobID, nil
}

// RestoreJobs lists restore jobs.
func (s *Service) RestoreJobs(ctx context.Context, accountID int64) ([]RestoreJobRow, error) {
	return listRestoreJobs(ctx, s.db, accountID, 50)
}

// ProfileArchive returns the read-only profile archive for a job.
func (s *Service) ProfileArchive(ctx context.Context, accountID, jobID int64) (*ProfileRow, error) {
	return findProfileSnapshot(ctx, s.db, accountID, jobID)
}

// DeleteArchive removes an archive.
func (s *Service) DeleteArchive(ctx context.Context, accountID, jobID int64) error {
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return deleteArchive(ctx, tx, accountID, jobID)
	})
}

// ExpireBackups is the worker entry: flip ready backups past expires_at.
func (s *Service) ExpireBackups(ctx context.Context) (int, error) {
	now := s.now()
	var ids []int64
	err := mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var err error
		ids, err = expireReadyBackups(ctx, tx, now)
		return err
	})
	return len(ids), err
}

// --- handshakes ---

// CreateHandshake starts a device-migration handshake.
func (s *Service) CreateHandshake(ctx context.Context, accountID int64) (int64, string, error) {
	code, err := sixDigitCode()
	if err != nil {
		return 0, "", err
	}
	now := s.now()
	var id int64
	err = mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		var err error
		id, err = insertHandshake(ctx, tx, accountID, code, now)
		return err
	})
	return id, code, err
}

// AcceptHandshake validates the code and moves the handshake to transferring.
func (s *Service) AcceptHandshake(ctx context.Context, accountID, id int64, code string) error {
	h, err := findHandshake(ctx, s.db, id, accountID)
	if err != nil {
		return err
	}
	if h.Status != "pending" {
		return apperrors.New(apperrors.StateConflict, "handshake not pending")
	}
	if h.Code != code {
		return apperrors.Unauth("invalid code")
	}
	if s.now().After(h.ExpiresAt) {
		return apperrors.New(apperrors.StateConflict, "handshake expired")
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markHandshakeAccepted(ctx, tx, id, now)
	})
}

// CompleteHandshake marks a transferring handshake as done.
func (s *Service) CompleteHandshake(ctx context.Context, accountID, id int64) error {
	if _, err := findHandshake(ctx, s.db, id, accountID); err != nil {
		return err
	}
	now := s.now()
	return mysqlx.WithinTx(ctx, s.db, func(tx mysqlx.Tx) error {
		return markHandshakeDone(ctx, tx, id, now)
	})
}

func sixDigitCode() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
	return fmt.Sprintf("%06d", n%1000000), nil
}
