// Command bench runs a benchmark experiment end to end and writes one raw JSON record per run.
//
//	bench e1 -api bin/api -out benchmark-scratch/2026-11-08-laptop-dev
//
// Every run starts the API as its own process, resets the database and the gate, seeds a fresh event,
// drives the open-loop load, collects the API metrics and the store load, then runs the independent
// checker and the reconciler on the database. The record embeds the environment and every parameter.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "e1":
		err = e1(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bench e1 [flags]   (E1: oversell test, every strategy, repeated runs)")
	os.Exit(2)
}
