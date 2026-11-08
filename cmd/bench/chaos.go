package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---- the chaos schedule (pure, so it can be tested) ----

type chaosKind string

const (
	kSigkill  chaosKind = "sigkill"          // the harness SIGKILLs the API at a random moment
	kHook     chaosKind = "hook_crash"       // the harness arms a crash hook; the next request that reaches it kills the API
	kRedis    chaosKind = "redis_restart"    // Redis is restarted (and loses its state: it runs without persistence)
	kPostgres chaosKind = "postgres_restart" // Postgres is killed and restarted
)

type chaosEvent struct {
	Kind  chaosKind     `json:"kind"`
	At    time.Duration `json:"at_ns"`           // offset from the start of the arrivals
	Point string        `json:"point,omitempty"` // for hook crashes: which named point is armed
}

// minGap is the least time between two chaos events, so the API (about a second to start) and the stores
// are back before the next one: each event then tests recovery from one failure, not an unrecoverable pile-up.
const minGap = 4 * time.Second

// hookPoints are the crash points armed during load (the rows of docs/crash-analysis.md that kill the API).
var hookPoints = []string{
	"hold.before_commit", "hold.before_reply", "confirm.after_marker", "confirm.during_payment",
	"confirm.after_payment", "confirm.before_commit", "confirm.before_reply",
}

// fastpathHookPoints exist only when the fast path is in use.
var fastpathHookPoints = []string{"hold.after_gate_claim", "hold.before_gate_extend"}

// buildSchedule places the events at random offsets inside the arrival window (between 15 and 80 percent of
// it, so there is load before the first failure and time to recover after the last), spaced at least minGap
// apart when the window allows. It is deterministic in seed.
func buildSchedule(sigkills, hooks, redis, postgres int, window time.Duration, fastpath bool, seed uint64) []chaosEvent {
	rng := rand.New(rand.NewPCG(seed, seed^0xa5a5a5a5))
	var kinds []chaosKind
	add := func(k chaosKind, n int) {
		for i := 0; i < n; i++ {
			kinds = append(kinds, k)
		}
	}
	add(kSigkill, sigkills)
	add(kHook, hooks)
	add(kRedis, redis)
	add(kPostgres, postgres)
	rng.Shuffle(len(kinds), func(i, j int) { kinds[i], kinds[j] = kinds[j], kinds[i] })
	if len(kinds) == 0 {
		return nil
	}
	lo, hi := time.Duration(0.15*float64(window)), time.Duration(0.80*float64(window))
	slot := (hi - lo) / time.Duration(len(kinds))
	points := hookPoints
	if fastpath {
		points = append(append([]string{}, hookPoints...), fastpathHookPoints...)
	}
	out := make([]chaosEvent, len(kinds))
	for i, k := range kinds {
		// A random point inside the slot, leaving half of minGap at its end when the slot is wide enough.
		margin := min(minGap/2, slot/4)
		at := lo + time.Duration(i)*slot + time.Duration(rng.Float64()*float64(slot-margin))
		out[i] = chaosEvent{Kind: k, At: at}
		if k == kHook {
			out[i].Point = points[rng.IntN(len(points))]
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// ---- a supervised API process ----

// supervisor runs the API as a real process on a fixed address and restarts it whenever it dies, whether
// the harness killed it or a crash hook made it exit. It counts the two causes apart.
type supervisor struct {
	h    *harness
	env  []string
	addr string
	base string

	mu          sync.Mutex
	cmd         *exec.Cmd
	harnessKill bool
	stopped     atomic.Bool
	wg          sync.WaitGroup

	Sigkills  atomic.Int64 // killed by the harness
	HookExits atomic.Int64 // exited by itself with status 137 (an armed crash hook fired)
	OtherExit atomic.Int64 // any other exit: the API could not reach Postgres, or crashed for another reason
	Restarts  atomic.Int64
}

func newSupervisor(h *harness, env []string) (*supervisor, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	s := &supervisor{h: h, env: env, addr: fmt.Sprintf("127.0.0.1:%d", port)}
	s.base = "http://" + s.addr
	return s, nil
}

// launch starts one process and waits until it answers; it fails if the process dies first.
func (s *supervisor) launch(ctx context.Context) error {
	cmd := exec.Command(s.h.apiBin)
	cmd.Env = append(os.Environ(), append(append([]string{}, s.env...), "HTTP_ADDR="+s.addr)...)
	if s.h.keepLogs {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cmd, s.harnessKill = cmd, false
	s.mu.Unlock()
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	ready := time.After(30 * time.Second)
	for {
		select {
		case <-exited:
			return fmt.Errorf("the API exited before it was ready")
		case <-ctx.Done():
			cmd.Process.Kill()
			return ctx.Err()
		case <-ready:
			cmd.Process.Kill()
			return fmt.Errorf("the API was not ready within 30 s")
		default:
		}
		if resp, err := http.Get(s.base + "/metrics"); err == nil {
			resp.Body.Close()
			s.wg.Add(1)
			go s.watch(ctx, cmd, exited)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// watch classifies the death of a process and brings a new one up.
func (s *supervisor) watch(ctx context.Context, cmd *exec.Cmd, exited <-chan struct{}) {
	defer s.wg.Done()
	<-exited
	if s.stopped.Load() {
		return
	}
	s.mu.Lock()
	byHarness := s.harnessKill
	s.mu.Unlock()
	switch {
	case byHarness:
		s.Sigkills.Add(1)
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 137:
		s.HookExits.Add(1)
	default:
		s.OtherExit.Add(1)
	}
	for !s.stopped.Load() {
		if err := s.launch(ctx); err == nil {
			s.Restarts.Add(1)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond): // Postgres may still be down: try again
		}
	}
}

// start brings the first process up.
func (s *supervisor) start(ctx context.Context) error { return s.launch(ctx) }

// sigkill kills the current process the way the operating system would, with no chance to clean up.
func (s *supervisor) sigkill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.harnessKill = true
		s.cmd.Process.Kill()
	}
}

// waitUp waits until the API answers (after a kill the supervisor restarts it in the background).
func (s *supervisor) waitUp(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(s.base + "/metrics"); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the API did not come back within %v", timeout)
}

func (s *supervisor) stop() {
	s.stopped.Store(true)
	s.mu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// ---- store restarts ----

// storeCommands restart Redis and Postgres. The defaults use Docker, which is right on a development
// machine; on a server set the flags to commands that restart the real services.
type storeCommands struct {
	redis, postgres string
}

func (c storeCommands) run(ctx context.Context, which string) error {
	spec := c.redis
	def := []string{"docker", "compose", "restart", "-t", "0", "redis"}
	if which == "postgres" {
		spec = c.postgres
		def = []string{"docker", "compose", "restart", "-t", "0", "postgres"} // -t 0: killed at once, not stopped politely
	}
	var cmd *exec.Cmd
	switch {
	case spec == "":
		cmd = exec.CommandContext(ctx, def[0], def[1:]...)
	case runtime.GOOS == "windows":
		cmd = exec.CommandContext(ctx, "cmd", "/C", spec)
	default:
		cmd = exec.CommandContext(ctx, "sh", "-c", spec)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restarting %s: %v: %s", which, err, strings.TrimSpace(string(out)))
	}
	return nil
}
