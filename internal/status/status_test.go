package status_test

import (
	"testing"

	"github.com/110y/muxac/internal/status"
)

func TestFromEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		event  string
		want   status.Status
		wantOK bool
	}{
		{name: "UserPromptSubmit", event: "UserPromptSubmit", want: status.Running, wantOK: true},
		{name: "PreToolUse", event: "PreToolUse", want: status.Running, wantOK: true},
		{name: "PermissionRequest", event: "PermissionRequest", want: status.Waiting, wantOK: true},
		{name: "Stop", event: "Stop", want: status.Stopped, wantOK: true},
		{name: "SessionStart", event: "SessionStart", want: status.Stopped, wantOK: true},
		{name: "SessionEnd", event: "SessionEnd", want: status.Stopped, wantOK: true},
		{name: "unknown event", event: "SomeOtherEvent", want: "", wantOK: false},
		{name: "empty event", event: "", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := status.FromEvent(tt.event)
			if ok != tt.wantOK {
				t.Errorf("FromEvent(%q) ok = %v, want %v", tt.event, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("FromEvent(%q) = %q, want %q", tt.event, got, tt.want)
			}
		})
	}
}

func TestIsValidTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current status.Status
		event   string
		want    bool
	}{
		{name: "unknown + UserPromptSubmit", current: status.Unknown, event: "UserPromptSubmit", want: true},
		{name: "unknown + Stop", current: status.Unknown, event: "Stop", want: true},
		{name: "waiting + Stop blocked", current: status.Waiting, event: "Stop", want: false},
		{name: "waiting + UserPromptSubmit", current: status.Waiting, event: "UserPromptSubmit", want: true},
		{name: "waiting + SessionEnd", current: status.Waiting, event: "SessionEnd", want: true},
		{name: "waiting + SessionStart", current: status.Waiting, event: "SessionStart", want: true},
		{name: "running + Stop", current: status.Running, event: "Stop", want: true},
		{name: "stopped + Stop", current: status.Stopped, event: "Stop", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := status.IsValidTransition(tt.current, tt.event)
			if got != tt.want {
				t.Errorf("IsValidTransition(%q, %q) = %v, want %v", tt.current, tt.event, got, tt.want)
			}
		})
	}
}
