package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"omp-telegram/internal/bridge"
	"omp-telegram/internal/config"
	"omp-telegram/internal/logging"
	"omp-telegram/internal/store"
)

var Version = "v0.6.1"

func displayVersion() string {
	revision := ""
	modified := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = strings.TrimSpace(setting.Value)
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
	}
	if revision == "" {
		return Version
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		revision += "-dirty"
	}
	return fmt.Sprintf("%s (%s)", Version, revision)
}

func main() {
	os.Exit(run())
}

func run() int {
	path := flag.String("config", "", "TOML configuration file (--config or -c; default: executable-adjacent config.toml, otherwise embedded defaults)")
	flag.StringVar(path, "c", "", "short form of --config")
	check := flag.Bool("check", false, "validate configuration without connecting (--check)")
	version := flag.Bool("version", false, "show application version and exit")
	flag.BoolVar(version, "v", false, "short form of --version")
	flag.Parse()
	if *version {
		fmt.Printf("omp-telegram %s\n", displayVersion())
		return 0
	}
	c, e := config.Load(*path)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return 1
	}
	if *check {
		fmt.Println("configuration valid")
		return 0
	}
	logs, e := logging.New(os.Stderr, logging.Options{
		Level: c.LogLevel, Format: c.LogFormat, ComponentLevels: c.LogComponentLevels,
	})
	if e != nil {
		fmt.Fprintln(os.Stderr, "invalid logging configuration")
		return 1
	}
	logger := logs.Logger(logging.Daemon)
	syscall.Umask(0077)
	lock, e := openDaemonLock(filepath.Join(c.DataDir, "daemon.lock"))
	if e != nil {
		logger.Error("daemon lock unavailable", "event", "lock_failed", "reason", "lock_unavailable")
		return 1
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		logger.Error("data directory already locked", "event", "lock_failed", "reason", "data_dir_locked")
		return 1
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	db, e := store.Open(c.DataDir)
	if e != nil {
		logger.Error("database initialization failed", "event", "daemon_fatal", "reason", "store_open_failed", "error_kind", "persistence")
		return 1
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("daemon started", "event", "daemon_start")
	if err := bridge.Run(ctx, c, db, logs); err != nil {
		// The owning bridge boundary has already recorded the specific failure.
		logger.Info("daemon stopped", "event", "daemon_stop", "result", "failed")
		return 1
	}
	logger.Info("daemon stopped", "event", "daemon_stop", "result", "done")
	return 0
}
