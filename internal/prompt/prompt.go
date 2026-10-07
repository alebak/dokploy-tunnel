// Package prompt decides how commands obtain values the user did not pass as
// flags: by asking interactively when input is allowed, or by failing with a
// missing_input error that names the flag to pass when it is not.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/alebak/dokploy-tunnel/internal/clierr"
)

// Request describes a value a command needs.
type Request struct {
	// Flag is the flag name, without dashes, that supplies the value
	// non-interactively.
	Flag string
	// Label is the human-readable name shown when asking.
	Label string
}

// Choice describes a selection of one or more options a command needs.
type Choice struct {
	// Label is the question shown above the options.
	Label string
	// Options are the choices, shown numbered from 1.
	Options []string
	// Missing is returned when no selection can be obtained, such as when
	// prompting is not allowed. It names the arguments or flags that make
	// the selection non-interactively and has code MissingInput.
	Missing *clierr.Error
}

// Input obtains missing values for commands.
type Input interface {
	// Ask returns the value for req, or a *clierr.Error with code
	// MissingInput when no value can be obtained.
	Ask(req Request) (string, error)
	// Choose returns the indexes of the options of c the user picks, in
	// ascending order and without repeats, or c.Missing when nothing is
	// picked. An answer that does not name options is an InvalidArgument
	// *clierr.Error.
	Choose(c Choice) ([]int, error)
}

// New returns the Input for the given policy. When allowed is false every
// request fails with MissingInput; otherwise questions are written to out and
// answers are read line by line from in.
func New(allowed bool, in io.Reader, out io.Writer) Input {
	if !allowed {
		return noInput{}
	}
	return &lineInput{in: bufio.NewReader(in), out: out}
}

// IsTerminal reports whether f is an interactive terminal (a character
// device), which is how doktunnel decides that --no-input is implied.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func missing(req Request) *clierr.Error {
	return clierr.Newf(clierr.MissingInput, "missing value for --%s", req.Flag).
		WithHint(fmt.Sprintf("pass --%s <value>", req.Flag))
}

type noInput struct{}

func (noInput) Ask(req Request) (string, error) {
	return "", missing(req)
}

func (noInput) Choose(c Choice) ([]int, error) {
	return nil, c.Missing
}

type lineInput struct {
	in  *bufio.Reader
	out io.Writer
}

func (l *lineInput) Ask(req Request) (string, error) {
	if _, err := fmt.Fprintf(l.out, "%s: ", req.Label); err != nil {
		return "", fmt.Errorf("writing prompt: %w", err)
	}
	line, err := l.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading answer for --%s: %w", req.Flag, err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return "", missing(req)
	}
	return answer, nil
}

func (l *lineInput) Choose(c Choice) ([]int, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", c.Label)
	for i, o := range c.Options {
		fmt.Fprintf(&b, "  %d) %s\n", i+1, o)
	}
	b.WriteString("Numbers or ranges, separated by commas or spaces, or all: ")
	if _, err := io.WriteString(l.out, b.String()); err != nil {
		return nil, fmt.Errorf("writing prompt: %w", err)
	}
	line, err := l.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading selection: %w", err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return nil, c.Missing
	}
	return parseSelection(answer, len(c.Options))
}

// parseSelection reads an answer such as "1, 3-4" or "all" choosing among n
// options numbered from 1, and returns the chosen indexes from 0.
func parseSelection(answer string, n int) ([]int, error) {
	if strings.EqualFold(answer, "all") {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		return all, nil
	}
	invalid := func(tok string) error {
		return clierr.Newf(clierr.InvalidArgument, "invalid selection %q", tok).
			WithHint(fmt.Sprintf("pick numbers from 1 to %d, ranges such as 1-%d, or all", n, n))
	}
	var picked []int
	for _, tok := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		from, to, isRange := strings.Cut(tok, "-")
		if !isRange {
			to = from
		}
		lo, err1 := strconv.Atoi(from)
		hi, err2 := strconv.Atoi(to)
		if err1 != nil || err2 != nil || lo < 1 || hi > n || lo > hi {
			return nil, invalid(tok)
		}
		for i := lo; i <= hi; i++ {
			picked = append(picked, i-1)
		}
	}
	slices.Sort(picked)
	return slices.Compact(picked), nil
}
