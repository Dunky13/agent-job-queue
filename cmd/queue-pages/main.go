package main

import (
	"os"

	"github.com/Dunky13/agent-job-queue/internal/pages"
)

func main() { os.Exit(pages.Main(os.Args[1:])) }
