// Command loadgen runs one open-loop load against a running API and writes the raw record of the
// run: summary.json (client-side figures) and buyers.ndjson (one line per buyer, used to reconcile
// with the database).
//
//	loadgen -url http://localhost:8080 -items 3000 -rate 200 -duration 20s -skew 1.1 -out out/run1
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/workload"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run() error {
	url := flag.String("url", "http://localhost:8080", "API base URL")
	items := flag.Int("items", 1000, "items in the event to create for this run")
	rate := flag.Float64("rate", 100, "buyers per second (open loop)")
	duration := flag.Duration("duration", 10*time.Second, "how long arrivals are sent")
	skew := flag.Float64("skew", 1.1, "Zipf exponent of demand (0 = uniform)")
	seed := flag.Uint64("seed", 1, "random seed, recorded so the demand can be repeated")
	name := flag.String("run", "", "run id (default: derived from the time)")
	out := flag.String("out", "", "directory for summary.json and buyers.ndjson (default: print only)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	eventID, itemIDs, err := loadgen.CreateEvent(ctx, *url, *items)
	if err != nil {
		return err
	}
	ev := struct {
		EventID int64
		ItemIDs []int64
	}{eventID, itemIDs}

	runID := *name
	if runID == "" {
		runID = fmt.Sprintf("run%d", time.Now().Unix())
	}
	res, err := loadgen.Run(ctx, loadgen.Config{
		BaseURL: *url, ItemIDs: ev.ItemIDs, Picker: workload.NewPicker(len(ev.ItemIDs), *skew, *seed),
		Rate: *rate, Duration: *duration, RunID: runID})
	if err != nil {
		return err
	}
	fmt.Print(loadgen.Format(res.Summary))
	fmt.Printf("  event %d (%d items), skew %.2f, seed %d\n", ev.EventID, len(ev.ItemIDs), *skew, *seed)

	if *out == "" {
		return nil
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	meta := map[string]any{"summary": res.Summary, "event_id": ev.EventID, "items": len(ev.ItemIDs), "skew": *skew, "seed": *seed, "url": *url}
	b, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "summary.json"), b, 0o644); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(*out, "buyers.ndjson"))
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range res.Buyers {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return w.Flush()
}
