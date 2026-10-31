// Command remote is the node agent CLI: node start|status|bench and the
// backend registry.
package main

import "remote.network/node/internal/cli"

func main() {
	cli.Execute()
}
