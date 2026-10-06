// Command genschema writes the workflow JSON Schema to a file.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ggrocco/icaro/internal/workflow"
)

func main() {
	out := flag.String("out", "schema/workflow.schema.json", "output path")
	flag.Parse()
	if err := os.WriteFile(*out, workflow.SchemaJSON(), 0o644); err != nil { //nolint:gosec // schema is public
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
