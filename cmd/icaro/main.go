package main

import (
	"os"

	"icaro/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
