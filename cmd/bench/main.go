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
	case "e2":
		err = e2(os.Args[2:])
	case "e3":
		err = e3(os.Args[2:])
	case "e4":
		err = e4(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bench e1|e2|e3|e4 [flags]\n  e1  oversell test, every strategy, repeated runs\n  e2  capacity and latency per strategy (ramp, confirmation, 70% latency, baseline first and last)\n  e3  surge protection: with and without the waiting room at several oversubscription factors\n  e4  failures under load: real API kills, Redis and Postgres restarts, an expiry race; judged by the checker and the reconciler")
	os.Exit(2)
}
