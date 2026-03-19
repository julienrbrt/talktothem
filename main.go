package main

import (
	"os"

	"github.com/julienrbrt/talktothem/cmd"
)

func main() {
	if err := cmd.NewRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}
