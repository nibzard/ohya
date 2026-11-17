package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

const backupStepPages = 128

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
	var pageSize int64
	if err := src.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
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
			backup, err := destinationConn.Backup("main", sourceConn, "main")
			if err != nil {
				return err
			}
			copyCtx, cancel := context.WithTimeout(ctx, backupBudget(int64(backup.PageCount())*pageSize))
			defer cancel()
			for {
				if err := copyCtx.Err(); err != nil {
					_ = backup.Close()
					return err
				}
				done, err := backup.Step(backupStepPages)
				if progress != nil && pageSize > 0 {
					total := int64(backup.PageCount()) * pageSize
					progress(total-int64(backup.Remaining())*pageSize, total)
				}
				if err != nil {
					_ = backup.Close()
					return err
				}
				if done {
					return backup.Close()
				}
				select {
				case <-copyCtx.Done():
					_ = backup.Close()
					return copyCtx.Err()
				case <-time.After(5 * time.Millisecond):
				}
			}
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

func backupBudget(totalBytes int64) time.Duration {
	budget := time.Duration(totalBytes/backupMinBytesPerSecond) * time.Second
	if budget < backupMinBudget {
		return backupMinBudget
	}
	return budget
}
