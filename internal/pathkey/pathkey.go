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
