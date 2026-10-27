// albert-backup runs in an independent controller container or on the Docker host.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"filippo.io/age"
	"openalbert/services/backup"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: albert-backup serve|request|create|list|restore|sync|monitor|units|keygen [options]")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	configPath := f.String("config", "/etc/openalbert-backup/config.json", "backup configuration")
	id := f.String("id", "", "copy ID from list")
	identity := f.String("identity", "", "offline age identity file (restore), or new file (keygen)")
	destination := f.String("destination", "", "new absolute restore directory")
	output := f.String("output", "/etc/systemd/system", "unit output directory")
	envPath := f.String("environment-file", "/etc/openalbert-backup/s3.env", "root-only file with BACKUP_S3_* credentials")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if args[0] == "keygen" {
		if *identity == "" {
			return errors.New("--identity must name a new key file; generate it on your recovery machine")
		}
		key, err := age.GenerateX25519Identity()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(*identity, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(file, key.String())
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		fmt.Println("Public recipient:", key.Recipient().String())
		return nil
	}
	c, err := backup.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	runner, err := backup.NewRunner(ctx, c)
	if err != nil {
		return err
	}
	if args[0] == "serve" {
		return runner.Serve(ctx, os.Stderr)
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	switch args[0] {
	case "request":
		if err = runner.Request(); err != nil {
			return err
		}
		fmt.Println("Backup requested; follow the backup container logs for its result.")
		return nil
	case "create":
		return runner.Create(ctx)
	case "sync":
		return runner.Sync()
	case "monitor":
		return runner.Monitor(time.Now())
	case "units":
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		if err = c.Units(binary, *configPath, *envPath, *output); err != nil {
			return err
		}
		if err = runner.Sync(); err != nil {
			return err
		}
		fmt.Println("Units written. Enable both timers after reviewing them. Next scheduled time:", c.NextRun(time.Now()).Format(time.RFC3339))
		return nil
	case "list", "restore":
		remote, err := backup.NewS3(ctx, c)
		if err != nil {
			return err
		}
		copies, err := remote.List(ctx)
		if err != nil {
			return err
		}
		if args[0] == "list" {
			return json.NewEncoder(os.Stdout).Encode(copies)
		}
		if *identity == "" || *id == "" || !filepath.IsAbs(*destination) {
			return errors.New("restore requires --id, --identity and an absolute --destination")
		}
		for _, copy := range copies {
			if copy.ID == *id {
				stage, err := os.MkdirTemp("", "albert-restore-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(stage)
				archive := filepath.Join(stage, "archive.age")
				if err = remote.Download(ctx, copy, archive); err != nil {
					return err
				}
				if err = backup.Restore(ctx, archive, *identity, *destination); err != nil {
					return err
				}
				fmt.Println("Restored to", *destination, "— services remain stopped. Review backup-manifest.json before starting.")
				return nil
			}
		}
		return errors.New("backup ID not found")
	default:
		return errors.New("unknown backup command")
	}
}
