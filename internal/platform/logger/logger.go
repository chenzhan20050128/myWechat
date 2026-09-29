// Package logger provides a thin structured logging facade over log/slog.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// ctxKey carries request-scoped fields (request_id, user_id) into log lines.
type ctxKey struct{}

// New builds an slog logger per WECHAT_LOG_LEVEL/WECHAT_LOG_FORMAT.
func New(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if strings.ToLower(format) == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// WithFields returns a context carrying extra log attributes.
func WithFields(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return context.WithValue(ctx, ctxKey{}, append(existing, attrs...))
}

// L returns the logger enriched with context fields.
func L(ctx context.Context, base *slog.Logger) *slog.Logger {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	if len(attrs) == 0 {
		return base
	}
	args := make([]any, 0, len(attrs))
	for _, a := range attrs {
		args = append(args, a)
	}
	return base.With(args...)
}
