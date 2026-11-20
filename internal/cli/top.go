// top.go: `remote node top` — the agent's newest
// live sample, read from ~/.remote/live.json. No listener, no new port:
// the running agent writes the file every 5 s (0600, atomic rename), this
// command reads it. Older than 15 s it says the agent is not running —
// a stale reading is not a live one.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"remote.network/node/wire"
)

// liveFile is live.json's shape: the heartbeat's stats object plus the
// per-router connection lines the fleet writes alongside it.
type liveFile struct {
	wire.NodeStats
	Conns []connLine `json:"conns,omitempty"`
}

type connLine struct {
	Name         string `json:"name"`
	Connected    bool   `json:"connected"`
	InFlightConn int    `json:"in_flight_conn"`
}

func newTopCommand() *cobra.Command {
	var (
		watch  time.Duration
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Show the running agent's live sample (machine, backends, router connections)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := homePath("live.json")
			for {
				if err := printTop(path, asJSON); err != nil {
					return err
				}
				if watch <= 0 {
					return nil
				}
				time.Sleep(watch)
				fmt.Println()
			}
		},
	}
	cmd.Flags().DurationVar(&watch, "watch", 0, "refresh this often (e.g. 2s), until interrupted")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw sample document")
	return cmd
}

func printTop(path string, asJSON bool) error {
	if asJSON {
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("no sample yet — is the agent running? (%s)", path)
		}
		fmt.Println(string(b))
		return nil
	}
	out, err := renderTop(path, time.Now)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

// renderTop reads the file and draws the view; now is injectable for the
// staleness test. A returned string keeps the rendering testable.
func renderTop(path string, now func() time.Time) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no sample yet — is the agent running? (%s)", path)
	}
	var doc liveFile
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("unreadable sample %s: %w", path, err)
	}
	at, err := time.Parse(time.RFC3339, doc.At)
	if err != nil {
		return "", fmt.Errorf("sample has no timestamp: %s", path)
	}
	age := now().Sub(at)
	if age < 0 {
		age = 0
	}
	if age > 15*time.Second {
		return fmt.Sprintf("agent not running (last sample %s, %s ago)\n",
			at.Local().Format("15:04:05"), age.Round(time.Second)), nil
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("sample %s · %s ago\n", at.Local().Format("15:04:05"), age.Round(time.Second)))
	if doc.CPU != nil {
		line := fmt.Sprintf("cpu  load %.2f ×%d", doc.CPU.Load1, doc.CPU.Cores)
		if doc.CPU.Util != nil {
			line += fmt.Sprintf(" · util %.0f%%", *doc.CPU.Util*100)
		}
		out.WriteString(line + "\n")
	}
	if doc.Mem != nil {
		out.WriteString(fmt.Sprintf("mem  %d/%d GiB used · %d GiB available\n",
			doc.Mem.UsedMB/1024, doc.Mem.TotalMB/1024, doc.Mem.AvailableMB/1024))
	}
	for _, g := range doc.GPUs {
		line := fmt.Sprintf("gpu %d", g.Index)
		if g.Util != nil {
			line += fmt.Sprintf(" · %.0f%%", *g.Util*100)
		}
		if g.MemUsedMB != nil && g.MemTotalMB != nil {
			line += fmt.Sprintf(" · %d/%d MiB", *g.MemUsedMB, *g.MemTotalMB)
		}
		if g.PowerW != nil {
			line += fmt.Sprintf(" · %.1fW", *g.PowerW)
		}
		if g.TempC != nil {
			line += fmt.Sprintf(" · %.0f°C", *g.TempC)
		}
		out.WriteString(line + "\n")
	}
	if len(doc.Backends) == 0 {
		out.WriteString("no backends (nothing probed yet)\n")
		return out.String(), nil
	}
	for _, b := range doc.Backends {
		line := fmt.Sprintf("backend %s (%s, via %s)  slots %d held %d busy 1m %d",
			b.ID, b.Kind, b.Source, b.Slots, b.Held, b.Busy1m)
		if b.Running != nil || b.Waiting != nil {
			line += fmt.Sprintf(" · running %d waiting %d",
				derefI(b.Running), derefI(b.Waiting))
		}
		if b.KV != nil {
			line += fmt.Sprintf(" · kv %.2f", *b.KV)
		}
		if b.DecodeTPS != nil {
			line += fmt.Sprintf(" · %.1f tok/s", *b.DecodeTPS)
		}
		if b.MemUsedMB != nil {
			line += fmt.Sprintf(" · %d MiB", *b.MemUsedMB)
		}
		out.WriteString(line + "\n")
	}
	for _, c := range doc.Conns {
		state := "disconnected"
		if c.Connected {
			state = "connected"
		}
		out.WriteString(fmt.Sprintf("router %s  %s · in-flight %d\n", c.Name, state, c.InFlightConn))
	}
	return out.String(), nil
}

func derefI(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
