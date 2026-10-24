package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAskStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := newTerm(strings.NewReader(""), io.Discard).ask(ctx, "x", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
