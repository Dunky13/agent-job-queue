package main

import (
	"os"

	"github.com/Dunky13/agent-job-queue/internal/remote"
)

func main() { os.Exit(remote.Main(os.Args[1:])) }
