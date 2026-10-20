package main

import (
	"os"

	"github.com/Dunky13/agent-job-queue/internal/preview"
)

func main() { os.Exit(preview.Main(os.Args[1:])) }
