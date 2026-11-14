package programstatus

import (
	"bytes"
	"strings"
	"testing"
)

// recorder is a Handler that records the events it receives in order.
type recorder struct {
	events []string
}

func (r *recorder) Query(terminator string) {
	r.events = append(r.events, "query "+quote(terminator))
}

func (r *recorder) Report(rep Report) {
	ev := "report " + string(rep.state)
	if rep.id != "" {
		ev += " id=" + rep.id
	}
	r.events = append(r.events, ev)
}

func quote(s string) string {
	return strings.NewReplacer("\x1b", `\e`, "\x07", `\a`).Replace(s)
}

func TestScanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantOutput string
		wantEvents []string
	}{
		{
			name:       "plain output passes through",
			input:      "hello, world\r\n",
			wantOutput: "hello, world\r\n",
		},
		{
			name:       "other escape sequences pass through",
			input:      "\x1b[1;31mred\x1b[0m\x1b]0;⠂ Claude Code\x07\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\x1b7\x1b8",
			wantOutput: "\x1b[1;31mred\x1b[0m\x1b]0;⠂ Claude Code\x07\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\x1b7\x1b8",
		},
		{
			name:       "OSC sequences that only share a prefix pass through",
			input:      "\x1b]75;x\x07\x1b]7502;state=working\x07\x1b]75010;y\x07",
			wantOutput: "\x1b]75;x\x07\x1b]7502;state=working\x07\x1b]75010;y\x07",
		},
		{
			name:       "BEL-terminated query is answered and stripped",
			input:      "\x1b]7501;?\x07\x1b[>0q\x1b[c",
			wantOutput: "\x1b[>0q\x1b[c",
			wantEvents: []string{`query \a`},
		},
		{
			name:       "ST-terminated query is answered and stripped",
			input:      "\x1b]7501;?\x1b\\\x1b[c",
			wantOutput: "\x1b[c",
			wantEvents: []string{`query \e\`},
		},
		{
			name:       "reports are stripped and delivered in order",
			input:      "a\x1b]7501;state=working:app=claude-code\x07b\x1b]7501;state=blocked:app=claude-code:kind=permission\x1b\\c",
			wantOutput: "abc",
			wantEvents: []string{"report working", "report blocked"},
		},
		{
			name:       "child records and clears",
			input:      "\x1b]7501;state=working:app=claude-code:id=t1\x07\x1b]7501;state=clear:id=t1\x07\x1b]7501;state=clear\x07",
			wantEvents: []string{"report working id=t1", "report clear id=t1", "report clear"},
		},
		{
			name:       "invalid reports are stripped but not delivered",
			input:      "x\x1b]7501;state=sleeping\x07y",
			wantOutput: "xy",
		},
		{
			name:       "ESC inside the payload ends the sequence and starts a new one",
			input:      "\x1b]7501;state=working\x1b[0mz",
			wantOutput: "\x1b[0mz",
		},
		{
			name:       "ESC ] inside the payload starts a new OSC",
			input:      "\x1b]7501;state=working\x1b]7501;state=idle\x07",
			wantEvents: []string{"report idle"},
		},
		{
			name:       "CAN cancels the sequence and reaches the terminal",
			input:      "\x1b]7501;state=working\x18after",
			wantOutput: "\x18after",
		},
		{
			name:       "oversized sequences are dropped",
			input:      "\x1b]7501;state=done:msg=" + strings.Repeat("A", maxSequenceBytes) + "\x07tail",
			wantOutput: "tail",
		},
		{
			name:       "UTF-8 text around reports is untouched",
			input:      "✳ 完了\x1b]7501;state=done\x07⏺",
			wantOutput: "✳ 完了⏺",
			wantEvents: []string{"report done"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Whole input in one chunk.
			var s Scanner
			var h recorder
			out := s.Scan(nil, []byte(tt.input), &h)
			out = s.Flush(out)
			assertScan(t, "single chunk", out, h.events, tt.wantOutput, tt.wantEvents)

			// One byte at a time: sequences split at every possible boundary
			// must give the same result.
			var s2 Scanner
			var h2 recorder
			var out2 []byte
			for i := range len(tt.input) {
				out2 = s2.Scan(out2, []byte{tt.input[i]}, &h2)
			}
			out2 = s2.Flush(out2)
			assertScan(t, "byte by byte", out2, h2.events, tt.wantOutput, tt.wantEvents)
		})
	}
}

func assertScan(t *testing.T, mode string, gotOutput []byte, gotEvents []string, wantOutput string, wantEvents []string) {
	t.Helper()

	if string(gotOutput) != wantOutput {
		t.Errorf("%s: output = %q, want %q", mode, gotOutput, wantOutput)
	}
	if strings.Join(gotEvents, "|") != strings.Join(wantEvents, "|") {
		t.Errorf("%s: events = %q, want %q", mode, gotEvents, wantEvents)
	}
}

// queryOrderHandler checks that a query is answered while the bytes written
// after it are still withheld from the terminal.
type queryOrderHandler struct {
	recorder
	outputSoFar *[]byte
	sawOutput   []byte
}

func (h *queryOrderHandler) Query(terminator string) {
	h.sawOutput = bytes.Clone(*h.outputSoFar)
	h.recorder.Query(terminator)
}

func TestScannerAnswersQueryBeforeLaterOutputIsForwarded(t *testing.T) {
	t.Parallel()

	var s Scanner
	var forwarded []byte
	h := &queryOrderHandler{outputSoFar: &forwarded}

	// Claude Code writes the query and the DA1 request in one write; the DA1
	// request must not reach the terminal before the query has been answered.
	forwarded = s.Scan(forwarded, []byte("before\x1b]7501;?\x07\x1b[c"), h)

	if len(h.events) != 1 {
		t.Fatalf("events = %q, want one query", h.events)
	}
	if bytes.Contains(h.sawOutput, []byte("\x1b[c")) {
		t.Errorf("DA1 request was forwarded before the query was answered: %q", h.sawOutput)
	}
	if string(forwarded) != "before\x1b[c" {
		t.Errorf("forwarded = %q, want %q", forwarded, "before\x1b[c")
	}
}

func TestScannerFlushReleasesWithheldPrefix(t *testing.T) {
	t.Parallel()

	var s Scanner
	var h recorder
	out := s.Scan(nil, []byte("text\x1b]750"), &h)
	if string(out) != "text" {
		t.Fatalf("output before flush = %q, want %q", out, "text")
	}
	out = s.Flush(out)
	if string(out) != "text\x1b]750" {
		t.Errorf("output after flush = %q, want %q", out, "text\x1b]750")
	}

	// An unterminated OSC 7501 sequence is dropped at the end of output.
	var s2 Scanner
	out = s2.Scan(nil, []byte("text\x1b]7501;state=working"), &h)
	out = s2.Flush(out)
	if string(out) != "text" {
		t.Errorf("output with unterminated report = %q, want %q", out, "text")
	}
	if len(h.events) != 0 {
		t.Errorf("events = %q, want none", h.events)
	}
}

func TestReply(t *testing.T) {
	t.Parallel()

	if got := string(Reply("\x07")); got != "\x1b]7501;?\x07" {
		t.Errorf("Reply(BEL) = %q", got)
	}
	if got := string(Reply("\x1b\\")); got != "\x1b]7501;?\x1b\\" {
		t.Errorf("Reply(ST) = %q", got)
	}
}
