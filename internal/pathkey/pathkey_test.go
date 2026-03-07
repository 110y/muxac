package pathkey

import (
	"testing"
)

func Test_fromDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{
			name: "simple path",
			dir:  "/home/user/project",
			want: "@home@user@project",
		},
		{
			name: "path with dots",
			dir:  "/home/user/.config/app",
			want: "@home@user@_config@app",
		},
		{
			name: "path with multiple dots",
			dir:  "/home/user/go/src/github.com/110y/muxac",
			want: "@home@user@go@src@github_com@110y@muxac",
		},
		{
			name: "root path",
			dir:  "/",
			want: "@",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := fromDir(tt.dir)
			if got != tt.want {
				t.Errorf("fromDir(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestClaudeProjectDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  string
		want string
	}{
		{
			name: "simple path without dots",
			dir:  "/home/user/project",
			want: "-home-user-project",
		},
		{
			name: "path with domain dot",
			dir:  "/home/user/go/src/github.com/org/repo",
			want: "-home-user-go-src-github-com-org-repo",
		},
		{
			name: "path with hidden directory dot",
			dir:  "/home/user/.config/app",
			want: "-home-user--config-app",
		},
		{
			name: "root path",
			dir:  "/",
			want: "-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClaudeProjectDir(tt.dir)
			if got != tt.want {
				t.Errorf("ClaudeProjectDir(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestTmuxSessionName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sn   string
		dir  string
		want string
	}{
		{
			name: "default name with simple path",
			sn:   "default",
			dir:  "/home/user/project",
			want: "muxac-default@home@user@project",
		},
		{
			name: "custom name with path",
			sn:   "foo",
			dir:  "/home/user/project",
			want: "muxac-foo@home@user@project",
		},
		{
			name: "root path",
			sn:   "default",
			dir:  "/",
			want: "muxac-default@",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TmuxSessionName(tt.sn, tt.dir)
			if got != tt.want {
				t.Errorf("TmuxSessionName(%q, %q) = %q, want %q", tt.sn, tt.dir, got, tt.want)
			}
		})
	}
}
