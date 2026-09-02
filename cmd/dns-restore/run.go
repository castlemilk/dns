package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/castlemilk/dns/internal/authoritative"
	"github.com/castlemilk/dns/internal/snapshot"
	"github.com/castlemilk/dns/internal/zone"
)

const maxSnapshotBytes int64 = 1 << 30

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return runWithClock(args, stdin, stdout, stderr, time.Now)
}

func runWithClock(args []string, stdin io.Reader, stdout, stderr io.Writer, now func() time.Time) error {
	flags := flag.NewFlagSet("dns-restore", flag.ContinueOnError)
	flags.SetOutput(stderr)
	snapshotPath := flags.String("snapshot", "", "snapshot JSON path, or - for stdin")
	databasePath := flags.String("database", "", "new or empty bbolt database path")
	verifyOnly := flags.Bool("verify-only", false, "validate the snapshot without opening a database")
	bumpSerials := flags.Bool("bump-serials", false, "select newer recovery SOA serials; use the latest verified backup and compare the last live serial")
	flags.Usage = func() {
		if _, err := fmt.Fprintln(stderr, "usage: dns-restore --snapshot PATH|- --database PATH [--bump-serials]"); err != nil {
			return
		}
		if _, err := fmt.Fprintln(stderr, "       dns-restore --snapshot PATH|- --verify-only"); err != nil {
			return
		}
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*snapshotPath) == "" {
		flags.Usage()
		return errors.New("--snapshot is required and positional arguments are not accepted")
	}
	if *verifyOnly {
		if strings.TrimSpace(*databasePath) != "" {
			return errors.New("--verify-only and --database are mutually exclusive")
		}
		if *bumpSerials {
			return errors.New("--verify-only and --bump-serials are mutually exclusive")
		}
	} else if strings.TrimSpace(*databasePath) == "" {
		flags.Usage()
		return errors.New("--database is required unless --verify-only is used")
	}

	raw, err := readSnapshot(*snapshotPath, stdin)
	if err != nil {
		return fmt.Errorf("read snapshot: %w", err)
	}
	document, err := snapshot.Decode(raw)
	if err != nil {
		return fmt.Errorf("verify snapshot checksum: %w", err)
	}
	if err := zone.ValidateSnapshot(document.Zones); err != nil {
		return fmt.Errorf("verify snapshot zones: %w", err)
	}
	if _, err := authoritative.Compile(document.Zones); err != nil {
		return fmt.Errorf("compile authoritative snapshot: %w", err)
	}
	records := 0
	for _, value := range document.Zones {
		records += len(value.Records)
	}
	restoreZones := document.Zones
	if *bumpSerials {
		restoreZones, err = zone.BumpSnapshotSerials(document.Zones, now().UTC())
		if err != nil {
			return fmt.Errorf("bump restored SOA serials: %w", err)
		}
		if _, err := authoritative.Compile(restoreZones); err != nil {
			return fmt.Errorf("compile serial-bumped snapshot: %w", err)
		}
	}
	status := "valid"
	if !*verifyOnly {
		store, err := zone.OpenEmptyForRestore(*databasePath)
		if err != nil {
			return fmt.Errorf("open restore target: %w", err)
		}
		if err := store.RestoreSnapshot(context.Background(), restoreZones); err != nil {
			return errors.Join(err, store.Close())
		}
		if err := store.Close(); err != nil {
			return err
		}
		status = "restored"
	}
	result := struct {
		Status        string `json:"status"`
		Zones         int    `json:"zones"`
		Records       int    `json:"records"`
		GeneratedAt   string `json:"generated_at"`
		Checksum      string `json:"checksum"`
		SerialsBumped bool   `json:"serials_bumped"`
	}{
		Status: status, Zones: len(document.Zones), Records: records,
		GeneratedAt: document.GeneratedAt.UTC().Format(time.RFC3339Nano), Checksum: document.Checksum, SerialsBumped: *bumpSerials,
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func readSnapshot(path string, stdin io.Reader) (raw []byte, resultErr error) {
	reader := stdin
	var file *os.File
	if path != "-" {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
		file, err = os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() {
			resultErr = errors.Join(resultErr, file.Close())
		}()
		reader = file
	}
	return readSnapshotBounded(reader, maxSnapshotBytes)
}

func readSnapshotBounded(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("snapshot exceeds the %d-byte limit", limit)
	}
	return raw, nil
}
