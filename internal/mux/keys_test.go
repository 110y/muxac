package mux

import "testing"

func TestKeyScanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix byte
		chunks []string
		want   string
		detach bool
	}{
		{name: "plain input", chunks: []string{"hello\r"}, want: "hello\r"},
		{name: "detach", chunks: []string{"ab\x02dcd"}, want: "ab", detach: true},
		{name: "detach across chunks", chunks: []string{"ab\x02", "d"}, want: "ab", detach: true},
		{name: "prefix twice sends it", chunks: []string{"a\x02\x02b"}, want: "a\x02b"},
		{name: "other keys after the prefix are ignored", chunks: []string{"\x02xy"}, want: "y"},
		{name: "escape sequence after the prefix is ignored", chunks: []string{"\x02\x1b[1;5Ax"}, want: "x"},
		{name: "SS3 sequence after the prefix is ignored", chunks: []string{"\x02\x1bOAx"}, want: "x"},
		{name: "UTF-8 character after the prefix is ignored", chunks: []string{"\x02日x"}, want: "x"},
		{name: "pasted prefix is passed on", chunks: []string{"\x1b[200~a\x02d\x1b[201~"}, want: "\x1b[200~a\x02d\x1b[201~"},
		{name: "prefix works after a paste", chunks: []string{"\x1b[200~a\x1b[2", "01~\x02d"}, want: "\x1b[200~a\x1b[201~", detach: true},
		{name: "another prefix detaches", prefix: 0x0f, chunks: []string{"a\x0fd"}, want: "a", detach: true},
		{name: "another prefix twice sends it", prefix: 0x0f, chunks: []string{"\x0f\x0f"}, want: "\x0f"},
		{name: "Ctrl+b with another prefix is plain input", prefix: 0x0f, chunks: []string{"\x02d"}, want: "\x02d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			k := keyScanner{prefix: tc.prefix}
			var got []byte
			detached := false
			for _, chunk := range tc.chunks {
				out, d := k.scan([]byte(chunk))
				got = append(got, out...)
				if d {
					detached = true
					break
				}
			}
			if string(got) != tc.want || detached != tc.detach {
				t.Errorf("scan = %q, detach %v; want %q, detach %v", got, detached, tc.want, tc.detach)
			}
		})
	}
}

func TestPrefixKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    byte
		wantErr bool
	}{
		{in: "", want: 0x02},
		{in: "C-b", want: 0x02},
		{in: "C-o", want: 0x0f},
		{in: "c-A", want: 0x01},
		{in: "^o", want: 0x0f},
		{in: "C-]", want: 0x1d},
		{in: "C-_", want: 0x1f},
		{in: "C-[", wantErr: true},
		{in: "C-1", wantErr: true},
		{in: "o", wantErr: true},
		{in: "M-o", wantErr: true},
		{in: "C-oo", wantErr: true},
	}
	for _, tc := range tests {
		got, err := prefixKey(tc.in)
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("prefixKey(%q) = %#x, %v; want %#x, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestParseProbeReplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want probeResult
		rest string
		done bool
	}{
		{
			name: "all the answers",
			in:   "\x1b]10;rgb:ffff/ffff/ffff\x1b\\\x1b]11;rgb:0000/0000/0000\x07\x1b[12;1R\x1b[?62;22c",
			want: probeResult{colors: [2]string{"rgb:ffff/ffff/ffff", "rgb:0000/0000/0000"}, cursor: [2]int{12, 1}},
			done: true,
		},
		{
			name: "DA1 only, with keys around",
			in:   "ab\x1b[?1;2cd",
			rest: "abd",
			done: true,
		},
		{
			name: "not answered yet",
			in:   "\x1b]11;rgb:0000/0000/0000\x07x",
			want: probeResult{colors: [2]string{"", "rgb:0000/0000/0000"}},
			rest: "x",
		},
		{
			name: "other escape sequences belong to the session",
			in:   "\x1b[A\x1b]12;x\x07\x1b[1;5R",
			want: probeResult{cursor: [2]int{1, 5}},
			rest: "\x1b[A\x1b]12;x\x07",
		},
		{
			name: "keys ending with R are not a position",
			in:   "\x1b[1;xR",
			rest: "\x1b[1;xR",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got probeResult
			rest, done := parseProbeReplies([]byte(tc.in), &got)
			if got != tc.want || string(rest) != tc.rest || done != tc.done {
				t.Errorf("parseProbeReplies = %+v, %q, %v; want %+v, %q, %v", got, rest, done, tc.want, tc.rest, tc.done)
			}
		})
	}
}
