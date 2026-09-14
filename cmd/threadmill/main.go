package main

import (
	"os"

	"github.com/KDZZZZZZ/threadmill/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.IO{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}))
}
