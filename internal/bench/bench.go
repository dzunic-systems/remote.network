// Package bench measures what a backend actually delivers — TTFT and decode
// tok/s from live generation, never spec sheets ("never trust specs"). Plan
// recipe: 3 fixed prompts, 128 output tokens, report TTFT p50 and mean
// decode rate.
package bench

import (
	"context"
	"fmt"
	"sort"
	"time"

	"remote.network/node/internal/backend"
)

// Parameters of every measurement.
const (
	Prompts         = 3
	MaxTokens       = 128
	MinDecodeDeltas = 4 // below this a run is too short to rate
)

// benchPrompts are fixed so numbers are comparable across nodes and runs.
var benchPrompts = []string{
	"Explain what a NAT gateway does, in simple terms.",
	"Write a short paragraph about tides.",
	"List five everyday materials and one use of each.",
}

// Result is one model's measured performance.
type Result struct {
	Model       string  `json:"model"`
	TTFTp50ms   int     `json:"ttft_ms_p50"`
	TokPerSec   float64 `json:"tok_per_sec"`
	Tokens      int     `json:"tokens"`       // total deltas counted
	Runs        int     `json:"runs"`         // successful runs
	Errors      int     `json:"errors"`       // failed runs
	UsageOutput int     `json:"usage_output"` // backend-reported output tokens (cross-check)
}

// Run benches one model on one backend. Errors on individual runs are
// counted, not fatal — a flaky backend gets the numbers it earned.
func Run(ctx context.Context, c backend.Client, model string) (*Result, error) {
	res := &Result{Model: model}
	var ttfts []time.Duration
	var rates []float64

	for _, prompt := range benchPrompts {
		if ctx.Err() != nil {
			break
		}
		ttft, rate, tokens, usageOut, err := runOne(ctx, c, model, prompt)
		if err != nil {
			res.Errors++
			continue
		}
		res.Runs++
		res.Tokens += tokens
		res.UsageOutput += usageOut
		ttfts = append(ttfts, ttft)
		if rate > 0 {
			rates = append(rates, rate)
		}
	}
	if res.Runs == 0 {
		return res, fmt.Errorf("bench %s: all %d runs failed", model, res.Errors)
	}

	sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })
	res.TTFTp50ms = int(ttfts[len(ttfts)/2].Milliseconds())
	if len(rates) > 0 {
		sum := 0.0
		for _, r := range rates {
			sum += r
		}
		res.TokPerSec = sum / float64(len(rates))
	}
	return res, nil
}

// runOne streams one prompt and measures: TTFT = first content delta;
// decode rate = deltas after the first / seconds between first and last.
func runOne(ctx context.Context, c backend.Client, model, prompt string) (ttft time.Duration, rate float64, tokens int, usageOutput int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	start := time.Now()
	var first time.Time
	err = c.StreamChat(ctx, backend.ChatRequest{
		Model: model,
		Messages: []backend.ChatMessage{
			{Role: "user", Content: prompt},
		},
		MaxTokens: MaxTokens,
	}, func(d backend.Delta) error {
		// Reasoning counts: thinking models spend real tokens/time on it.
		if d.Content != "" || d.Reasoning != "" {
			tokens++
			if first.IsZero() {
				first = time.Now()
			}
		}
		if d.Usage != nil && d.Usage.OutputTokens > 0 {
			usageOutput = d.Usage.OutputTokens
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if first.IsZero() || tokens < MinDecodeDeltas {
		return 0, 0, tokens, usageOutput, nil
	}
	ttft = first.Sub(start)
	elapsed := time.Since(first).Seconds()
	if elapsed > 0 {
		rate = float64(tokens-1) / elapsed
	}
	return ttft, rate, tokens, usageOutput, nil
}

// String renders one result for CLI output.
func (r *Result) String() string {
	cross := ""
	if r.UsageOutput > 0 {
		cross = fmt.Sprintf(" (backend claims %d out)", r.UsageOutput)
	}
	errNote := ""
	if r.Errors > 0 {
		errNote = fmt.Sprintf(", %d errored", r.Errors)
	}
	return fmt.Sprintf("%-28s TTFT p50 %5d ms   %6.1f tok/s   %d tokens%s%s",
		r.Model, r.TTFTp50ms, r.TokPerSec, r.Tokens, cross, errNote)
}
