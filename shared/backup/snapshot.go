package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"
)

func sqliteDSN(path string, readOnly bool) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := "_pragma=busy_timeout(15000)"
	if readOnly {
		query += "&mode=ro"
	}
	u.RawQuery = query
	return u.String()
}

// snapshotSQLite creates a consistent online SQLite snapshot using VACUUM INTO
// without stopping writers or copying live WAL/SHM files.
func snapshotSQLite(ctx context.Context, sourcePath, targetPath string) (err error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("sqlite database must be a regular file without symlinks")
	}
	if _, statErr := os.Lstat(targetPath); !errors.Is(statErr, os.ErrNotExist) {
		if statErr == nil {
			return errors.New("snapshot target already exists")
		}
		return statErr
	}
	defer func() {
		if err != nil {
			_ = os.Remove(targetPath)
		}
	}()
	db, err := sql.Open("sqlite", sqliteDSN(sourcePath, false))
	if err != nil {
		return fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	_, execErr := db.ExecContext(ctx, "VACUUM INTO ?", targetPath)
	closeErr := db.Close()
	if execErr != nil {
		return fmt.Errorf("sqlite VACUUM INTO failed: %w", execErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close sqlite database: %w", closeErr)
	}
	if err = os.Chmod(targetPath, 0600); err != nil {
		return err
	}
	return verifySQLite(ctx, targetPath)
}

func verifySQLite(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("sqlite database is missing, non-regular, or empty")
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, true))
	if err != nil {
		return fmt.Errorf("open sqlite snapshot: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("sqlite quick_check failed: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("read sqlite quick_check result: %w", err)
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite quick_check failed: %w", err)
	}
	if len(results) != 1 || !strings.EqualFold(results[0], "ok") {
		return errors.New("sqlite integrity check failed")
	}
	return nil
}

func checkDiskSpace(dir string, required uint64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return err
	}
	if stat.Bsize <= 0 {
		return errors.New("cannot determine staging filesystem block size")
	}
	if required > uint64(stat.Bavail)*uint64(stat.Bsize) {
		return errors.New("insufficient staging disk space")
	}
	return nil
}
