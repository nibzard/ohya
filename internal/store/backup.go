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

func Backup(ctx context.Context, source, destination string) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
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
			for {
				if err := ctx.Err(); err != nil {
					_ = backup.Close()
					return err
				}
				done, err := backup.Step(128)
				if err != nil {
					_ = backup.Close()
					return err
				}
				if done {
					return backup.Close()
				}
				select {
				case <-ctx.Done():
					_ = backup.Close()
					return ctx.Err()
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
