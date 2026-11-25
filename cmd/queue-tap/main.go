package main

import (
	"os"

	"github.com/Dunky13/agent-job-queue/internal/tap"
)

func main() { os.Exit(tap.Main(os.Args[1:])) }
