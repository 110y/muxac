//go:build !linux

package mux

// closeInheritedOnExec marks the descriptors from 3 on as closed on exec
// (see Client.startServer).
func closeInheritedOnExec() error {
	return markListedOnExec("/dev/fd")
}
