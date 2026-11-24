package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

const backupStepPages = 128

const backupStepYield = time.Millisecond

const backupMinBytesPerSecond = 256 << 10

const backupMinBudget = 2 * time.Minute

func Backup(ctx context.Context, source, destination string, progress func(done, total int64)) (err error) {
	original, err := OpenReadOnly(source)
	if err != nil {
		return err
	}
	defer original.Close()
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	target, err := sql.Open("sqlite3", dsn(destination, nil))
	if err != nil {
		return err
	}
	defer target.Close()
	dst, err := target.Conn(ctx)
	if err != nil {
		return err
	}
	defer dst.Close()
	src, err := original.readers.Conn(ctx)
	if err != nil {
		return err
	}
	defer src.Close()
	pageSize, pageCount, err := sourcePageMetrics(ctx, src)
	if err != nil {
		return fmt.Errorf("store: backup: %w", err)
	}
	err = dst.Raw(func(dstRaw any) error {
		return src.Raw(func(srcRaw any) error {
			destinationConn, ok := dstRaw.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("backup destination is not a SQLite connection")
			}
			sourceConn, ok := srcRaw.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("backup source is not a SQLite connection")
			}
			copyCtx, cancel := context.WithTimeout(ctx, backupBudget(pageCount*pageSize))
			defer cancel()
			var report func(copied, total int64)
			if progress != nil && pageSize > 0 {
				report = func(copied, total int64) {
					progress(copied*pageSize, total*pageSize)
				}
			}
			return copySnapshot(copyCtx, destinationConn, sourceConn, report)
		})
	})
	if err != nil {
		return fmt.Errorf("store: backup: %w", err)
	}
	if _, err = dst.ExecContext(ctx, "PRAGMA journal_mode=DELETE"); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func copySnapshot(ctx context.Context, destination, source *sqlite3.SQLiteConn, progress func(copied, total int64)) (err error) {
	read, err := source.Begin()
	if err != nil {
		return fmt.Errorf("begin the source read: %w", err)
	}
	if _, err := source.Exec(`SELECT count(*) FROM sqlite_schema`, nil); err != nil {
		_ = read.Rollback()
		return fmt.Errorf("take the source snapshot: %w", err)
	}
	defer func() {
		if read.Rollback() != nil {
			err = errors.Join(err, driver.ErrBadConn)
		}
	}()
	backup, err := destination.Backup("main", source, "main")
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = backup.Close()
			return err
		}
		done, err := backup.Step(backupStepPages)
		if progress != nil {
			progress(int64(backup.PageCount()-backup.Remaining()), int64(backup.PageCount()))
		}
		if err != nil {
			_ = backup.Close()
			return err
		}
		if done {
			return backup.Close()
		}
		timer := time.NewTimer(backupStepYield)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = backup.Close()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func sourcePageMetrics(ctx context.Context, conn *sql.Conn) (pageSize, pageCount int64, err error) {
	if err := conn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, err
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, 0, err
	}
	return pageSize, pageCount, nil
}

func backupBudget(totalBytes int64) time.Duration {
	budget := time.Duration(totalBytes/backupMinBytesPerSecond) * time.Second
	if budget < backupMinBudget {
		return backupMinBudget
	}
	return budget
}
