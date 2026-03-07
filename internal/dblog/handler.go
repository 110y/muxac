package dblog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/timestamp"
)

type handler struct {
	queries *sqlc.Queries
	level   slog.Leveler
	attrs   []slog.Attr
	groups  []string
}

func NewHandler(queries *sqlc.Queries, level slog.Leveler) *handler {
	return &handler{
		queries: queries,
		level:   level,
	}
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)

	prefix := strings.Join(h.groups, ".")
	for _, a := range h.attrs {
		h.appendAttr(&b, prefix, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&b, prefix, a)
		return true
	})

	dbCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	if err := h.queries.InsertDebugLog(dbCtx, sqlc.InsertDebugLogParams{
		Level:     r.Level.String(),
		Message:   b.String(),
		CreatedAt: r.Time.UTC().Format(timestamp.Format),
	}); err != nil {
		return fmt.Errorf("insert debug log: %w", err)
	}

	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{
		queries: h.queries,
		level:   h.level,
		attrs:   append(sliceClone(h.attrs), attrs...),
		groups:  sliceClone(h.groups),
	}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{
		queries: h.queries,
		level:   h.level,
		attrs:   sliceClone(h.attrs),
		groups:  append(sliceClone(h.groups), name),
	}
}

func (h *handler) appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	fmt.Fprintf(b, " %s=%v", key, a.Value)
}

func sliceClone[T any](s []T) []T {
	if s == nil {
		return nil
	}
	c := make([]T, len(s))
	copy(c, s)
	return c
}
