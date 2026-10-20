package main

import (
	"os"

	"github.com/Dunky13/agent-job-queue/internal/delivery"
)

func main() { os.Exit(delivery.Main(os.Args[1:])) }
