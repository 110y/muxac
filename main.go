package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
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
	"github.com/110y/muxac/internal/version"
)

//go:embed db/migrations/*.sql
var migrationsFS embed.FS

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

	if os.Args[1] == "--help" || os.Args[1] == "-h" {
		usage()
		return nil
	}

	if os.Args[1] == "version" {
		for _, arg := range os.Args[2:] {
			if arg == "--help" || arg == "-h" {
				usageVersion()
				return nil
			}
		}
		fmt.Fprintf(os.Stdout, "%s\n", version.Version)
		return nil
	}

	homeDir := os.Getenv("HOME")
	cacheBase := os.Getenv("XDG_CACHE_HOME")
	if cacheBase == "" {
		cacheBase = filepath.Join(homeDir, ".cache")
	}
	cacheDir := filepath.Join(cacheBase, "muxac")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	migrations, err := fs.Sub(migrationsFS, "db/migrations")
	if err != nil {
		return err
	}

	queries, conn, err := database.Open(ctx, migrations, cacheDir)
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
			case "--help", "-h":
				usageNew()
				return nil
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
			fi, err := os.Stat(workDir) //nolint:gosec // path is sanitized via filepath.Abs and filepath.Clean above
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
		return newcmd.Run(ctx, tmuxRunner, queries, name, workDir, tmuxConf, command, cacheDir, env)

	case "attach":
		name := "default"
		var dir string
		args := os.Args[2:]
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--help", "-h":
				usageAttach()
				return nil
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
			case "--help", "-h":
				usageList()
				return nil
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
		for _, arg := range os.Args[2:] {
			if arg == "--help" || arg == "-h" {
				usageHook()
				return nil
			}
		}
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
		for _, arg := range os.Args[2:] {
			if arg == "--help" || arg == "-h" {
				usageMonitor()
				return nil
			}
		}
		logger := slog.New(dblog.NewHandler(queries, slog.LevelError))
		return monitor.Run(ctx, tmuxRunner, queries, homeDir, cacheDir, logger)

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
  hook                                                                Update status based on coding agent hook event (reads JSON from stdin)
  version                                                             Show version information`)
}

func usageNew() {
	fmt.Fprintln(os.Stderr, `Usage: muxac new [options] <command>

Create a new tmux session and run the specified command.

Options:
  --name <name>         Session name (default: "default")
  --dir <path>          Working directory (default: current directory)
  --env KEY=VALUE       Set environment variable (can be specified multiple times)
  --tmux-conf <path>    Path to a custom tmux configuration file
  --help, -h            Show this help message`)
}

func usageAttach() {
	fmt.Fprintln(os.Stderr, `Usage: muxac attach [options]

Attach to an existing tmux session.

Options:
  --name <name>    Session name (default: "default")
  --dir <path>     Working directory (default: current directory)
  --help, -h       Show this help message`)
}

func usageList() {
	fmt.Fprintln(os.Stderr, `Usage: muxac list [options]

List all muxac sessions with their status.

Options:
  --no-header    Omit the header row from output
  --json         Output in JSON format
  --help, -h     Show this help message`)
}

func usageHook() {
	fmt.Fprintln(os.Stderr, `Usage: muxac hook

Update session status based on a coding agent hook event.
Reads a JSON hook event from stdin.

Required environment variables:
  MUXAC_SESSION_NAME    The tmux session name
  CLAUDE_PROJECT_DIR    The project directory for tool detection

Options:
  --help, -h    Show this help message`)
}

func usageMonitor() {
	fmt.Fprintln(os.Stderr, `Usage: muxac monitor

Run the background monitor process that tracks session status.

Options:
  --help, -h    Show this help message`)
}

func usageVersion() {
	fmt.Fprintln(os.Stderr, `Usage: muxac version

Show the muxac version.

Options:
  --help, -h    Show this help message`)
}
