package main

import (
	"io"
	"log/slog"
)

func newTestLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
