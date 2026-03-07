package pathkey

import "strings"

// fromDir converts a directory path to a path key by replacing "/" with "@" and "." with "_".
func fromDir(dir string) string {
	s := strings.ReplaceAll(dir, "/", "@")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// TmuxSessionName returns the tmux session name for a given name and directory.
func TmuxSessionName(name, dir string) string {
	return "muxac-" + name + fromDir(dir)
}

// ClaudeProjectDir converts a directory path to Claude Code's project directory encoding
// by replacing "/" with "-" (e.g., "/home/user/project" becomes "-home-user-project").
func ClaudeProjectDir(dir string) string {
	s := strings.ReplaceAll(dir, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}
