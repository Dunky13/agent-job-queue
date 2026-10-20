package main

import (
	"github.com/Dunky13/agent-job-queue/internal/backend"
	"os"
)

func main() { os.Exit(backend.MetadataMain(os.Args[1:])) }
