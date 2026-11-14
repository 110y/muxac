package programstatus

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseReport(t *testing.T) {
	t.Parallel()

	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	tests := []struct {
		name    string
		payload string
		want    Report
		wantOK  bool
	}{
		{
			name:    "root working report from Claude Code",
			payload: "state=working:app=claude-code",
			want:    Report{state: stateWorking},
			wantOK:  true,
		},
		{
			name:    "blocked with kind and msg",
			payload: "state=blocked:app=claude-code:kind=permission:msg=" + b64("Bash(rm -rf /tmp/x)"),
			want:    Report{state: stateBlocked},
			wantOK:  true,
		},
		{
			name:    "done with summary",
			payload: "state=done:app=claude-code:msg=" + b64("Wrote 3 files"),
			want:    Report{state: stateDone},
			wantOK:  true,
		},
		{
			name:    "child record",
			payload: "state=working:app=claude-code:id=a1b2c3:title=" + b64("Explore the repo"),
			want:    Report{state: stateWorking, id: "a1b2c3"},
			wantOK:  true,
		},
		{
			name:    "clear with id",
			payload: "state=clear:id=a1b2c3",
			want:    Report{state: stateClear, id: "a1b2c3"},
			wantOK:  true,
		},
		{
			name:    "clear without id",
			payload: "state=clear",
			want:    Report{state: stateClear},
			wantOK:  true,
		},
		{
			name:    "error with message",
			payload: "state=error:msg=" + b64("API overloaded"),
			want:    Report{state: stateError},
			wantOK:  true,
		},
		{
			name:    "spaces around keys and values are stripped",
			payload: " state = working : id = t1 ",
			want:    Report{state: stateWorking, id: "t1"},
			wantOK:  true,
		},
		{
			name:    "repeated key takes the last value",
			payload: "state=working:state=idle",
			want:    Report{state: stateIdle},
			wantOK:  true,
		},
		{
			name:    "unknown kind does not drop the report",
			payload: "state=blocked:kind=coffee",
			want:    Report{state: stateBlocked},
			wantOK:  true,
		},
		{
			name:    "unknown keys are ignored",
			payload: "state=idle:future=1",
			want:    Report{state: stateIdle},
			wantOK:  true,
		},
		{
			name:    "malformed pairs are skipped",
			payload: "state=working:noequals:=empty:Upper=1:key=bad;value",
			want:    Report{state: stateWorking},
			wantOK:  true,
		},
		{
			name:    "unpadded base64 is accepted",
			payload: "state=done:msg=" + strings.TrimRight(b64("ok!"), "="),
			want:    Report{state: stateDone},
			wantOK:  true,
		},
		{
			name:    "missing state drops the report",
			payload: "kind=permission",
		},
		{
			name:    "unknown state drops the report",
			payload: "state=sleeping",
		},
		{
			name:    "state with a disallowed byte drops the report",
			payload: "state=work ing",
		},
		{
			name:    "malformed id drops the report",
			payload: "state=working:id=a//b",
		},
		{
			name:    "id segment over 32 bytes drops the report",
			payload: "state=working:id=" + strings.Repeat("a", 33),
		},
		{
			name:    "id deeper than 8 levels drops the report",
			payload: "state=working:id=a/b/c/d/e/f/g/h/i",
		},
		{
			name:    "msg with a disallowed byte is a skipped pair",
			payload: "state=done:msg=@@@",
			want:    Report{state: stateDone},
			wantOK:  true,
		},
		{
			name:    "invalid base64 msg drops the report",
			payload: "state=done:msg=QQ=Q",
		},
		{
			name:    "truncated base64 msg drops the report",
			payload: "state=done:msg=Q",
		},
		{
			name:    "msg with a control character drops the report",
			payload: "state=done:msg=" + b64("line1\nline2"),
		},
		{
			name:    "msg with a C1 control character drops the report",
			payload: "state=done:msg=" + b64("a\u0085b"),
		},
		{
			name:    "msg over the decoded limit drops the report",
			payload: "state=done:msg=" + b64(strings.Repeat("x", 2049)),
		},
		{
			name:    "title over the encoded limit drops the report",
			payload: "state=working:title=" + strings.Repeat("A", 257),
		},
		{
			name:    "key over 16 bytes drops the report",
			payload: "state=idle:abcdefghijklmnopq=1",
		},
		{
			name:    "app over 32 bytes drops the report",
			payload: "state=idle:app=" + strings.Repeat("a", 33),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parseReport(tt.payload)
			if ok != tt.wantOK {
				t.Fatalf("parseReport(%q) ok = %v, want %v", tt.payload, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("parseReport(%q) = %+v, want %+v", tt.payload, got, tt.want)
			}
		})
	}
}
