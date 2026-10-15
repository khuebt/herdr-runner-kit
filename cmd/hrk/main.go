// Command hrk runs coding, research and report tasks as herdr runners.
package main

import (
	"fmt"
	"os"

	"github.com/khuebt/herdr-runner-kit/internal/cli"
)

func main() {
	if err := cli.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "hrk:", err)
		os.Exit(1)
	}
}
