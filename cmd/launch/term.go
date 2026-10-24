package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errStopped = errors.New("stopped by the operator")

// term is the operator's terminal.
type term struct {
	in  *bufio.Reader
	out io.Writer
}

func newTerm(in io.Reader, out io.Writer) term {
	return term{in: bufio.NewReader(in), out: out}
}

func (t term) say(format string, args ...any) {
	fmt.Fprintf(t.out, format+"\n", args...)
}

// ask reads one line. An empty answer is def.
// A cancelled ctx returns immediately. The operator can stop the window with Ctrl-C.
func (t term) ask(ctx context.Context, prompt, def string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if def != "" {
		fmt.Fprintf(t.out, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(t.out, "%s: ", prompt)
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := t.in.ReadString('\n')
		ch <- result{line, err}
	}()
	var line string
	var err error
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		line, err = r.line, r.err
	}
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", fmt.Errorf("read answer: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// yes asks until the answer is yes or no.
func (t term) yes(ctx context.Context, question string) (bool, error) {
	for {
		answer, err := t.ask(ctx, question+" (yes/no)", "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "yes", "y":
			return true, nil
		case "no", "n":
			return false, nil
		}
	}
}

// confirm stops the run unless the answer is yes.
func (t term) confirm(ctx context.Context, question string) error {
	ok, err := t.yes(ctx, question)
	if err != nil {
		return err
	}
	if !ok {
		return errStopped
	}
	return nil
}

// wait shows what to do by hand and returns when the operator presses Enter.
func (t term) wait(ctx context.Context, lines ...string) error {
	t.say("")
	for _, line := range lines {
		t.say("  %s", line)
	}
	_, err := t.ask(ctx, "Press Enter when done", "")
	return err
}
