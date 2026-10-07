package catalog

import (
	"log/slog"
	"strings"
)

func writeBlock(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	slog.Info(text)
}
