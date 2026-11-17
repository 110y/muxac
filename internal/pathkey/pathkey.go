package pathkey

import "strings"

// fromDir converts a directory path to a path key by replacing "/" with "@" and "." with "_".
func fromDir(dir string) string {
	s := strings.ReplaceAll(dir, "/", "@")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// SessionName returns the name of the multiplexer session for a given name and directory.
func SessionName(name, dir string) string {
	return "muxac-" + name + fromDir(dir)
}
