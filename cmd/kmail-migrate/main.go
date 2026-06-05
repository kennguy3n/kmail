// Command kmail-migrate is the schema-migration entrypoint (WS4
// Task 4). It wraps internal/schemamigrate with a tiny CLI:
//
//	kmail-migrate up                 apply all pending migrations
//	kmail-migrate down [N]           roll back the last N (default 1)
//	kmail-migrate status             print applied/pending state
//
// Connection comes from DATABASE_URL (same contract as
// scripts/migrate.sh). The migrations directory defaults to
// ./migrations and can be overridden with -dir.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/schemamigrate"
)

func main() {
	dir := flag.String("dir", "migrations", "migrations directory")
	flag.Parse()

	logger := log.New(os.Stderr, "[kmail-migrate] ", log.LstdFlags)

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd := args[0]

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgresql://kmail:kmail@localhost:5432/kmail"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	connCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(connCtx, dsn)
	if err != nil {
		logger.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	runner := schemamigrate.NewRunner(pool, *dir, logger)

	switch cmd {
	case "up":
		if err := runner.Up(ctx); err != nil {
			logger.Fatalf("up: %v", err)
		}
	case "down":
		steps := 1
		if len(args) > 1 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n <= 0 {
				logger.Fatalf("down: invalid step count %q", args[1])
			}
			steps = n
		}
		if err := runner.Down(ctx, steps); err != nil {
			logger.Fatalf("down: %v", err)
		}
	case "status":
		rows, err := runner.Status(ctx)
		if err != nil {
			logger.Fatalf("status: %v", err)
		}
		for _, r := range rows {
			state := "pending"
			if r.Applied {
				state = "applied"
			}
			down := ""
			if r.HasDown {
				down = " (down available)"
			}
			fmt.Printf("%-50s %s%s\n", r.Filename, state, down)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `kmail-migrate — schema migration runner

usage:
  kmail-migrate up            apply all pending migrations
  kmail-migrate down [N]      roll back the last N migrations (default 1)
  kmail-migrate status        show applied/pending migrations

flags:
  -dir string   migrations directory (default "migrations")

env:
  DATABASE_URL  postgres connection string
`)
}
