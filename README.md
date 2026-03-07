# muxac

**Multiplexer for Agentic Coding tools**

## Overview

`muxac` provides three primitive features to manage sessions of Agentic Coding tools:

- `muxac new`: Creating a new session
- `muxac list`: Listing all sessions with their status
- `muxac attach`: Attaching to an existing session

Using these primitives, you can also build your own tools like a dashboard or UI on top of `muxac`.

For example, the creator of `muxac` uses it with [fzf](https://github.com/junegunn/fzf) and [Neovim](https://github.com/neovim/neovim) to switch between sessions of Claude Code while previewing them as shown below:

[![asciicast](https://asciinema.org/a/Nfg2NY8Jhdf9cMvy.svg)](https://asciinema.org/a/Nfg2NY8Jhdf9cMvy)

## Usage

```bash
# Create a new Agentic Coding session for the current directory with an Agentic Coding tool command like `claude`.
$ muxac new claude

# You can detach from the session by pressing `Ctrl+b d`.

# List all sessions with their status.
$ muxac list

DIRECTORY               NAME     STATUS
/path/to/workspace-1    default  running
/path/to/workspace-2    foo      waiting
/path/to/workspace-3    bar      stopped

# Attach to an existing session for the current directory.
$ muxac attach
```

| Status | Description |
|--------|-------------|
| `running` | The agent is actively processing |
| `waiting` | The agent is waiting for a user response |
| `stopped` | The agent has stopped or the session is idle |

## Installation

Download the binary from the [release page](https://github.com/110y/muxac/releases).

### Prerequisites

- [tmux](https://github.com/tmux/tmux)

### Setup

<details>
<summary>Claude Code</summary>

Add the following hook configuration to your Claude Code settings file (e.g. `~/.claude/settings.json`):

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ],
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ],
    "PermissionRequest": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "muxac hook"
          }
        ]
      }
    ]
  }
}
```

</details>

<details>
<summary>Codex (Not yet supported, coming soon...)</summary>
</details>

<details>
<summary>Gemini CLI (Not yet supported, coming soon...)</summary>
</details>

<details>
<summary>GitHub Copilot CLI (Not yet supported, coming soon...)</summary>
</details>

<details>
<summary>OpenCode (Not yet supported, coming soon...)</summary>
</details>

## Command Reference

### `muxac new`

Creates a new tmux session and launches the specified agentic coding tool.

```bash
$ muxac new [--name <name>] [--dir <path>] [--env KEY=VALUE ...] [--tmux-conf <path>] <command>
```

| Flag | Description |
|------|-------------|
| `--name <name>` | Session name (default: `default`) |
| `--dir <path>` | Working directory (default: current directory) |
| `--env KEY=VALUE` | Environment variables to pass to the session (can be specified multiple times) |
| `--tmux-conf <path>` | Path to a tmux config file to source after session creation |

### `muxac list`

Lists all `muxac` sessions with their status.

```bash
$ muxac list [--no-header] [--json]
```

| Flag | Description |
|------|-------------|
| `--no-header` | Omit the header row |
| `--json` | Output in JSON format |

```bash
$ muxac list
DIRECTORY          NAME     STATUS
/home/user/myapp   default  running
/home/user/api     backend  waiting
```

### `muxac attach`

Attaches to an existing tmux session.

```bash
$ muxac attach [--name <name>] [--dir <path>]
```

| Flag | Description |
|------|-------------|
| `--name <name>` | Session name (default: `default`) |
| `--dir <path>` | Working directory (default: current directory) |
