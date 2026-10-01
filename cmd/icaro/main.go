package main

import (
	"os"

	"github.com/ggrocco/icaro/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
