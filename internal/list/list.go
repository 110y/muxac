package list

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/status"
)

// Options controls which columns to include in the output.
type Options struct {
	NoHeader bool
	JSON     bool
}

// jsonOutput is the top-level JSON structure for --json output.
type jsonOutput struct {
	Sessions []jsonEntry `json:"sessions"`
}

// jsonEntry represents a single session in JSON output.
type jsonEntry struct {
	Directory string `json:"directory"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

// entry represents a single session entry for display.
type entry struct {
	name      string
	status    status.Status
	path      string
	updatedAt string
}

// sortEntries sorts entries by directory then by session name.
func sortEntries(entries []entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].path != entries[j].path {
			return entries[i].path < entries[j].path
		}
		return entries[i].name < entries[j].name
	})
}

// formatEntries writes formatted entries to the writer as tab-aligned columns.
func formatEntries(w io.Writer, entries []entry, opts Options) {
	if len(entries) == 0 {
		return
	}

	pathWidth := len("DIRECTORY")
	nameWidth := len("NAME")
	statusWidth := len("STATUS")
	for _, e := range entries {
		if len(e.path) > pathWidth {
			pathWidth = len(e.path)
		}
		if len(e.name) > nameWidth {
			nameWidth = len(e.name)
		}
		if len(e.status) > statusWidth {
			statusWidth = len(e.status)
		}
	}

	if !opts.NoHeader {
		header := fmt.Sprintf("%-*s  %-*s  %-*s  UPDATED", pathWidth, "DIRECTORY", nameWidth, "NAME", statusWidth, "STATUS")
		fmt.Fprintln(w, header)
	}

	for _, e := range entries {
		line := fmt.Sprintf("%-*s  %-*s  %-*s  %s", pathWidth, e.path, nameWidth, e.name, statusWidth, string(e.status), e.updatedAt)
		fmt.Fprintln(w, line)
	}
}

// Run executes the list command: reads sessions from DB and outputs results.
func Run(ctx context.Context, w io.Writer, queries *sqlc.Queries, opts Options) error {
	dbSessions, err := queries.ListSessions(ctx)
	if err != nil {
		return err
	}

	var entries []entry
	for _, sess := range dbSessions {
		entries = append(entries, entry{
			name:      sess.Name,
			status:    status.Status(sess.Status),
			path:      sess.Path,
			updatedAt: sess.UpdatedAt,
		})
	}

	sortEntries(entries)

	if opts.JSON {
		jsonEntries := make([]jsonEntry, len(entries))
		for i, e := range entries {
			jsonEntries[i] = jsonEntry{
				Directory: e.path,
				Name:      e.name,
				Status:    string(e.status),
				UpdatedAt: e.updatedAt,
			}
		}
		out := jsonOutput{Sessions: jsonEntries}
		return json.NewEncoder(w).Encode(out)
	}

	if len(entries) == 0 {
		return nil
	}

	formatEntries(w, entries, opts)

	return nil
}
