package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/attach"
	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/dblog"
	"github.com/110y/muxac/internal/hook"
	"github.com/110y/muxac/internal/list"
	"github.com/110y/muxac/internal/monitor"
	"github.com/110y/muxac/internal/newcmd"
	"github.com/110y/muxac/internal/tmux"
)

//go:embed db/schema.sql
var ddl string

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("no command specified")
	}

	homeDir := os.Getenv("HOME")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	queries, conn, err := database.Open(ctx, ddl)
	if err != nil {
		return err
	}
	defer conn.Close()

	tmuxRunner := &tmux.ExecRunner{}

	switch os.Args[1] {
	case "new":
		name := "default"
		var dir string
		var env []string
		var command string
		var tmuxConf string
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--name":
				if i+1 >= len(args) {
					return fmt.Errorf("--name requires an argument")
				}
				name = args[i+1]
				i++
			case "--dir":
				if i+1 >= len(args) {
					return fmt.Errorf("--dir requires an argument")
				}
				dir = args[i+1]
				i++
			case "--env":
				if i+1 >= len(args) {
					return fmt.Errorf("--env requires a KEY=VALUE argument")
				}
				env = append(env, args[i+1])
				i++
			case "--tmux-conf":
				if i+1 >= len(args) {
					return fmt.Errorf("--tmux-conf requires an argument")
				}
				tmuxConf = args[i+1]
				i++
			default:
				if command == "" {
					command = args[i]
				}
			}
		}
		if command == "" {
			usage()
			return fmt.Errorf("new requires a command argument")
		}
		var workDir string
		if dir != "" {
			workDir, err = filepath.Abs(dir)
			if err != nil {
				return err
			}
			workDir = filepath.Clean(workDir)
			fi, err := os.Stat(workDir) //nolint:gosec // workDir is sanitized via filepath.Abs and filepath.Clean.
			if err != nil {
				return fmt.Errorf("--dir %q: %w", dir, err)
			}
			if !fi.IsDir() {
				return fmt.Errorf("--dir %q is not a directory", dir)
			}
		} else {
			workDir, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		if err := monitor.EnsureRunning(ctx, tmuxRunner, queries); err != nil {
			return err
		}
		return newcmd.Run(ctx, tmuxRunner, queries, name, workDir, tmuxConf, command, env)

	case "attach":
		name := "default"
		var dir string
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--name":
				if i+1 >= len(args) {
					return fmt.Errorf("--name requires an argument")
				}
				name = args[i+1]
				i++
			case "--dir":
				if i+1 >= len(args) {
					return fmt.Errorf("--dir requires an argument")
				}
				dir = args[i+1]
				i++
			}
		}
		var workDir string
		if dir != "" {
			workDir, err = filepath.Abs(dir)
			if err != nil {
				return err
			}
		} else {
			workDir, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		if err := monitor.EnsureRunning(ctx, tmuxRunner, queries); err != nil {
			return err
		}
		return attach.Run(ctx, tmuxRunner, name, workDir)

	case "list":
		var opts list.Options
		for _, arg := range os.Args[2:] {
			switch arg {
			case "--no-header":
				opts.NoHeader = true
			case "--json":
				opts.JSON = true
			}
		}
		if err := monitor.EnsureRunning(ctx, tmuxRunner, queries); err != nil {
			return err
		}
		return list.Run(ctx, os.Stdout, queries, opts)

	case "hook":
		sessionName := os.Getenv(agent.EnvSessionName)
		if sessionName == "" {
			return fmt.Errorf("%s is not set", agent.EnvSessionName)
		}
		claudeProjectDir := os.Getenv("CLAUDE_PROJECT_DIR")
		tool := agent.DetectTool(claudeProjectDir)
		if tool == agent.Unknown {
			return fmt.Errorf("unknown agentic coding tool")
		}
		projectDir := agent.ProjectDir(tool, claudeProjectDir)
		if err := monitor.EnsureRunning(ctx, tmuxRunner, queries); err != nil {
			return err
		}
		return hook.Run(ctx, os.Stdin, queries, sessionName, projectDir, tool)

	case "monitor":
		logger := slog.New(dblog.NewHandler(queries, slog.LevelError))
		return monitor.Run(ctx, tmuxRunner, queries, homeDir, logger)

	default:
		usage()
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: muxac <command>

Commands:
  new [--name <name>] [--dir <path>] [--env KEY=VALUE ...] [--tmux-conf <path>] <command>  Create a new tmux session
  attach [--name <name>] [--dir <path>]                               Attach to an existing tmux session
  list [--no-header] [--json]                                          List all muxac sessions with their status
  hook                                                                Update status based on coding agent hook event (reads JSON from stdin)`)
}
