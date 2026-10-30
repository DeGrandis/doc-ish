// Package recipes runs small Starlark programs that turn a file the
// reader can only sample (a device's export of thousands of readings)
// into values. The model writes a recipe once; a person approves it; after
// that every file in the same format is read by the recipe alone.
//
// Starlark is a small dialect of Python built for running untrusted
// configuration: it has no files, network, clock or randomness unless the
// host provides them. A recipe gets the file's text and a few helpers, can
// only hand back values through emit, and is stopped after a fixed number
// of steps or when its time runs out.
package recipes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Record is one value a recipe found.
type Record struct {
	Name    string   `json:"name"`
	Value   *float64 `json:"value,omitempty"`
	Text    string   `json:"text,omitempty"`
	Unit    string   `json:"unit,omitempty"`
	Date    string   `json:"date"` // YYYY-MM-DD or YYYY-MM-DDTHH:MM
	RefLow  *float64 `json:"ref_low,omitempty"`
	RefHigh *float64 `json:"ref_high,omitempty"`
	Flag    string   `json:"flag,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// Limits bound one run.
type Limits struct {
	Steps   uint64 // Starlark execution steps
	Records int    // values a run may emit
}

// DefaultLimits are generous for a year of readings every five minutes,
// and small enough that a runaway loop stops in seconds.
var DefaultLimits = Limits{Steps: 200_000_000, Records: 250_000}

// Output is what a run produced.
type Output struct {
	Records []Record
	Printed []string // what the recipe printed, for debugging a draft
}

// Run executes script against text.
func Run(ctx context.Context, script, text string, lim Limits) (Output, error) {
	var out Output
	thread := &starlark.Thread{
		Name: "recipe",
		Print: func(_ *starlark.Thread, msg string) {
			if len(out.Printed) < 50 {
				out.Printed = append(out.Printed, msg)
			}
		},
		Load: func(*starlark.Thread, string) (starlark.StringDict, error) {
			return nil, errors.New("recipes can't load other files")
		},
	}
	thread.SetMaxExecutionSteps(lim.Steps)
	thread.SetLocal("text", text) // what csv_rows() and csv_dicts() read by default
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			thread.Cancel("it ran out of time")
		case <-done:
		}
	}()

	emit := starlark.NewBuiltin("emit", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var (
			name, unit, date, text, flag, note string
			value, low, high                   starlark.Value = starlark.None, starlark.None, starlark.None
		)
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name, "value?", &value, "unit?", &unit, "date?", &date,
			"text?", &text, "low?", &low, "high?", &high, "flag?", &flag, "note?", &note); err != nil {
			return nil, err
		}
		if len(out.Records) >= lim.Records {
			return nil, fmt.Errorf("more than %d values; emit fewer (a daily average, say)", lim.Records)
		}
		r := Record{Name: strings.TrimSpace(name), Unit: strings.TrimSpace(unit), Date: strings.TrimSpace(date),
			Text: strings.TrimSpace(text), Flag: strings.TrimSpace(flag), Note: strings.TrimSpace(note)}
		var err error
		if r.Value, err = toFloat(value); err != nil {
			return nil, fmt.Errorf("emit: value: %w", err)
		}
		if r.RefLow, err = toFloat(low); err != nil {
			return nil, fmt.Errorf("emit: low: %w", err)
		}
		if r.RefHigh, err = toFloat(high); err != nil {
			return nil, fmt.Errorf("emit: high: %w", err)
		}
		out.Records = append(out.Records, r)
		return starlark.None, nil
	})

	predeclared := starlark.StringDict{
		"text":      starlark.String(text),
		"emit":      emit,
		"csv_rows":  starlark.NewBuiltin("csv_rows", csvRows),
		"csv_dicts": starlark.NewBuiltin("csv_dicts", csvDicts),
		"number":    starlark.NewBuiltin("number", numberFn),
		"date":      starlark.NewBuiltin("date", dateFn),
		"datetime":  starlark.NewBuiltin("datetime", datetimeFn),
		"json":      json.Module,
	}
	opts := &syntax.FileOptions{Set: true, While: true, TopLevelControl: true, GlobalReassign: true}
	_, err := starlark.ExecFileOptions(opts, thread, "recipe.star", script, predeclared)
	if err != nil {
		var ee *starlark.EvalError
		if errors.As(err, &ee) {
			return out, errors.New(ee.Backtrace())
		}
		return out, err
	}
	return out, nil
}

func toFloat(v starlark.Value) (*float64, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Int:
		f := float64(x.Float())
		return &f, nil
	case starlark.Float:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, nil
		}
		return &f, nil
	case starlark.String:
		return parseNumber(string(x)), nil
	}
	return nil, fmt.Errorf("want a number or None, got %s", v.Type())
}
