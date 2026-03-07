package status

// Status represents the current state of a coding agent session.
type Status string

const (
	Running Status = "running"
	Waiting Status = "waiting"
	Stopped Status = "stopped"
	Unknown Status = "unknown"
)

// FromEvent maps a canonical hook event name to a Status.
// Returns the status and true if the event is recognized, or empty and false otherwise.
func FromEvent(event string) (Status, bool) {
	switch event {
	case "UserPromptSubmit", "PreToolUse":
		return Running, true
	case "PermissionRequest":
		return Waiting, true
	case "Stop", "SessionStart", "SessionEnd":
		return Stopped, true
	default:
		return "", false
	}
}

// IsValidTransition checks whether transitioning from current based on the
// given event is allowed.
func IsValidTransition(current Status, event string) bool {
	if current == Waiting && event == "Stop" {
		return false
	}
	return true
}
