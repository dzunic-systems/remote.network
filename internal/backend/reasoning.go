// reasoning.go: turning the wire's thinking control into what a backend will
// take, and reporting what it really did. The openai kinds cover servers with
// different vocabularies (measured Nov-03):
//
//	            none   enable_thinking  low medium high  xhigh  unknown value
//	vLLM        off    off              ok  ok     400   ok     400
//	Ollama      off    ignored          ok  ok     ok    ok     400
//	llama.cpp   off    off              passed to the chat template, no check
//	LM Studio   off    ignored          ok  ok     ok    ok     400
//
// So a refused rung is not an error: StreamChat walks down the ladder to the
// nearest level the server accepts and remembers it per model. Acceptance is
// not obedience either (deepseek-r1 and gpt-oss on Ollama take "none" and think
// anyway; llama.cpp and LM Studio take levels the model has no use for), so the
// ladder is cut to what the server lists for the model when it lists anything,
// and a model that keeps thinking through "off" gets its lowest level instead.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"remote.network/node/wire"
)

// reasoningRung is one way of asking a server for a thinking level.
type reasoningRung struct {
	applied string // the level this rung asks for; "" = the backend's default
	effort  string // reasoning_effort
	kwargs  map[string]any
}

var thinkingOffKwargs = map[string]any{"enable_thinking": false}

// reasoningOptions is what a server says one model supports. Listed false means
// it said nothing, and any level is worth asking for.
type reasoningOptions struct {
	listed bool
	levels []string
	noOff  bool
}

func (o reasoningOptions) allows(level string) bool {
	if !o.listed {
		return true
	}
	for _, l := range o.levels {
		if l == level {
			return true
		}
	}
	return false
}

// reasoningLadder lists the rungs for a requested effort, nearest first, always
// ending in the backend's default so a request is never lost to its thinking
// control. "off" asks both ways at once: "none" is what Ollama and LM Studio
// honour and enable_thinking is what a server that refuses "none" may still
// honour. Past those, the nearest thing to off is the lowest level.
func reasoningLadder(requested string, o reasoningOptions) []reasoningRung {
	var names []string
	var out []reasoningRung
	switch requested {
	case "off":
		if !o.noOff {
			out = []reasoningRung{
				{applied: "off", effort: "none", kwargs: thinkingOffKwargs},
				{applied: "off", kwargs: thinkingOffKwargs},
				{applied: "off", effort: "none"},
			}
		}
		for _, n := range []string{"low", "medium", "high"} {
			if o.allows(n) {
				names = []string{n}
				break
			}
		}
	case "low":
		names = []string{"low", "medium", "high"}
	case "medium":
		names = []string{"medium", "low", "high"}
	case "high":
		names = []string{"high", "xhigh", "medium", "low"}
	case "xhigh":
		names = []string{"xhigh", "high", "medium", "low"}
	}
	for _, n := range names {
		if o.allows(n) {
			out = append(out, reasoningRung{applied: n, effort: n})
		}
	}
	return append(out, reasoningRung{})
}

// offThoughtLimit is how many requests in a row must think through "off" before a
// model loses it. One is not enough: gemma4 with tools thinks only some of the
// time, and losing "off" for good over a stray thought would bring back the
// classifier timeouts it exists to prevent.
const offThoughtLimit = 3

// noteOff counts thoughts through "off" and reports when the limit is reached; a
// request that stayed off starts the count over.
func (c *openaiClient) noteOff(key string, thought bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !thought {
		delete(c.offThoughts, key)
		return false
	}
	if c.offThoughts == nil {
		c.offThoughts = map[string]int{}
	}
	c.offThoughts[key]++
	return c.offThoughts[key] >= offThoughtLimit
}

// firstRungPast is the nearest rung that is not "off", for a model that thought
// through it.
func firstRungPast(ladder []reasoningRung) int {
	for i, r := range ladder {
		if r.applied != "off" {
			return i
		}
	}
	return len(ladder) - 1
}

// reasoningRungKey identifies a request shape in the per-model memory. Tools are
// part of it: gemma4 on LM Studio honours "none" alone and thinks when tools ride.
func reasoningRungKey(model, requested string, tools bool) string {
	return fmt.Sprintf("%s\x00%s\x00%t", model, requested, tools)
}

func rungID(r reasoningRung) string {
	return fmt.Sprintf("%s|%s|%t", r.applied, r.effort, r.kwargs != nil)
}

// startRung is the rung to begin at: the one that last worked for this request
// shape if the ladder still has it, else the nearest.
func (c *openaiClient) startRung(key string, ladder []reasoningRung) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.rungWorked[key]
	if !ok {
		return 0
	}
	for i, r := range ladder {
		if rungID(r) == id {
			return i
		}
	}
	return 0
}

func (c *openaiClient) rememberRung(key string, r reasoningRung) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rungWorked == nil {
		c.rungWorked = map[string]string{}
	}
	c.rungWorked[key] = rungID(r)
}

// reasoningOptionsFor reads what the server lists for a model: LM Studio's
// per-model options, or the levels a llama.cpp chat template renders. llama.cpp
// honours "none" whatever the template lists, so only LM Studio's list can rule
// "off" out.
func (c *openaiClient) reasoningOptionsFor(model string) reasoningOptions {
	c.mu.Lock()
	propsDone, nativeDone := c.propsDone, c.nativeDone
	c.mu.Unlock()
	if !propsDone {
		c.loadProps()
	}
	if !nativeDone {
		c.loadNativeOptions()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if options, ok := c.nativeOptions[model]; ok {
		o := reasoningOptions{listed: true, levels: options, noOff: true}
		for _, l := range options {
			if l == "off" {
				o.noOff = false
			}
		}
		return o
	}
	if c.propsLevelsKnown {
		return reasoningOptions{listed: true, levels: c.propsLevels}
	}
	return reasoningOptions{}
}

// loadNativeOptions reads LM Studio's model listing. Any other 4xx is final (not
// LM Studio); a transport error or 5xx is retried on the next call.
func (c *openaiClient) loadNativeOptions() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/api/v1/models", nil)
	if err != nil {
		return
	}
	c.auth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < 500 {
			c.mu.Lock()
			c.nativeDone = true
			c.mu.Unlock()
		}
		return
	}
	var out struct {
		Models []struct {
			Key             string `json:"key"`
			LoadedInstances []struct {
				ID string `json:"id"`
			} `json:"loaded_instances"`
			Capabilities struct {
				Reasoning *struct {
					AllowedOptions []string `json:"allowed_options"`
				} `json:"reasoning"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return
	}
	options := map[string][]string{}
	for _, m := range out.Models {
		if r := m.Capabilities.Reasoning; r != nil {
			options[m.Key] = r.AllowedOptions
			for _, inst := range m.LoadedInstances {
				options[inst.ID] = r.AllowedOptions
			}
		}
	}
	c.mu.Lock()
	c.nativeOptions, c.nativeDone = options, true
	c.mu.Unlock()
}

// thinkingSeen watches a stream for evidence of thinking: a reasoning field, or
// a literal <think> opening the content on a server that does not parse it.
type thinkingSeen struct {
	reasoning bool
	content   strings.Builder
}

func (t *thinkingSeen) observe(d Delta) {
	if d.Reasoning != "" {
		t.reasoning = true
	}
	if t.content.Len() < 16 {
		t.content.WriteString(d.Content)
	}
}

func (t *thinkingSeen) thought() bool {
	return t.reasoning || strings.HasPrefix(strings.TrimSpace(t.content.String()), "<think>")
}

// reasoningOutcome reports a control that was not met exactly; nil when it was.
func reasoningOutcome(requested string, rung reasoningRung, thought bool) *wire.ReasoningOutcome {
	applied := rung.applied
	if applied == "" || (applied == "off" && thought) {
		applied = "default" // nothing accepted, or took "off" and thought anyway
	}
	if applied == requested {
		return nil
	}
	return &wire.ReasoningOutcome{Requested: requested, Applied: applied}
}
