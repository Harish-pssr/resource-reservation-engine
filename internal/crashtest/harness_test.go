// Package crashtest runs the real-failure tests of docs/crash-analysis.md: it builds the real API
// binary, starts it as a separate process, arms a fault hook that makes the process EXIT at an exact
// point, drives the request, watches the process actually die, restarts it, and checks what a client
// retrying sees and what the database holds. Nothing here is simulated: the exit is os.Exit(137) in a
// real process, and Postgres sees a real dropped connection.
package crashtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

const adminToken = "crash-test-admin-token"

var apiBinary string

// TestMain builds the API binary once for every test in the package.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "crashtest-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tempdir:", err)
		os.Exit(2)
	}
	name := "api"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	apiBinary = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", apiBinary, "resourcereservation/cmd/api")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the API failed: %v\n%s", err, out)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// reps is how many times each scenario runs: 1 by default, CRASH_REPS=5 for the full evidence run.
func reps() int {
	if n, err := strconv.Atoi(os.Getenv("CRASH_REPS")); err == nil && n > 0 {
		return n
	}
	return 1
}

// server is one real API process.
type server struct {
	t    *testing.T
	cmd  *exec.Cmd
	base string
	done chan struct{}
	mu   sync.Mutex
	logs bytes.Buffer
	code int
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start launches the API with the given environment and waits until it answers.
func start(t *testing.T, env []string) *server {
	t.Helper()
	port := freePort(t)
	s := &server{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}
	s.cmd = exec.Command(apiBinary)
	s.cmd.Env = append(os.Environ(), append(env, fmt.Sprintf("HTTP_ADDR=127.0.0.1:%d", port))...)
	s.cmd.Stdout, s.cmd.Stderr = &lockedWriter{s}, &lockedWriter{s}
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start api: %v", err)
	}
	go func() {
		err := s.cmd.Wait()
		s.mu.Lock()
		if s.cmd.ProcessState != nil {
			s.code = s.cmd.ProcessState.ExitCode()
		} else if err != nil {
			s.code = -1
		}
		s.mu.Unlock()
		close(s.done)
	}()
	t.Cleanup(s.kill)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			t.Fatalf("the API exited while starting (code %d):\n%s", s.code, s.logText())
		default:
		}
		if resp, err := http.Get(s.base + "/metrics"); err == nil {
			resp.Body.Close()
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the API did not become ready:\n%s", s.logText())
	return nil
}

type lockedWriter struct{ s *server }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	return w.s.logs.Write(p)
}

func (s *server) logText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logs.String()
}

// waitExit waits for the process to die by itself and returns its exit status.
func (s *server) waitExit(timeout time.Duration) (int, bool) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.code, true
	case <-time.After(timeout):
		return 0, false
	}
}

func (s *server) kill() {
	select {
	case <-s.done:
	default:
		s.cmd.Process.Kill()
		<-s.done
	}
}

// ---- a minimal client ----

type response struct {
	status int
	body   map[string]any
	err    error // a transport error: the connection broke before an answer arrived
}

func (s *server) call(method, path string, body any, token string) response {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return response{status: resp.StatusCode, body: out}
}

// paymentAttempts is how many payments THIS process has started (it resets when the process restarts).
func (s *server) paymentAttempts() float64 {
	r := s.call("GET", "/metrics", nil, "")
	if r.err != nil {
		s.t.Fatalf("reading metrics: %v", r.err)
	}
	return r.body["outcomes"].(map[string]any)["payment_attempts"].(float64)
}

func (s *server) arm(point string) {
	s.t.Helper()
	r := s.call("PUT", "/admin/faults/"+point, map[string]any{"action": "crash", "remaining": 1}, adminToken)
	if r.err != nil || r.status != http.StatusNoContent {
		s.t.Fatalf("arming %s: %+v", point, r)
	}
}

func (s *server) hold(buyer, req string, item int64) response {
	return s.call("POST", "/holds", map[string]any{"buyer_id": buyer, "request_id": req, "item_ids": []int64{item}}, "")
}

func refsOf(r response) []map[string]any {
	var refs []map[string]any
	if raw, ok := r.body["items"].([]any); ok {
		for _, it := range raw {
			m := it.(map[string]any)
			refs = append(refs, map[string]any{"item_id": m["item_id"], "hold_version": m["hold_version"]})
		}
	}
	return refs
}

func (s *server) confirm(buyer string, refs []map[string]any) response {
	return s.call("POST", "/holds/confirm", map[string]any{"buyer_id": buyer, "items": refs}, "")
}

// baseEnv is the environment of every server in a scenario. The fault table lives in the database,
// so a restarted server finds the same (already consumed) settings.
func baseEnv(dbURL, strategy, prefix string) []string {
	return []string{
		"DATABASE_URL=" + dbURL, "STRATEGY=" + strategy, "SCHEMA=protected", "PAYMENT_PROFILE=fast", "PAYMENT_FAIL_PCT=0", "PAYMENT_TIMEOUT_PCT=0", "HOLD_TTL=60s",
		"FAULTS_ENABLED=true", "ADMIN_TOKEN=" + adminToken,
		"GATE_PREFIX=" + prefix + "g:", "ROOM_PREFIX=" + prefix + "r:", fmt.Sprintf("DB_POOL_SIZE=%d", 8),
	}
}

func newPrefix() string { return fmt.Sprintf("c%08x:", rand.Uint32()) }

// record appends one line per scenario run to CRASH_RESULTS, when set: the raw evidence of how many
// real kills were made and whether each scenario's assertions held.
func record(t *testing.T, row, point, strategy string, rep, exitCode int, killed bool) {
	path := os.Getenv("CRASH_RESULTS")
	if path == "" {
		return
	}
	t.Cleanup(func() {
		line, _ := json.Marshal(map[string]any{"row": row, "point": point, "strategy": strategy, "rep": rep,
			"killed": killed, "exit_code": exitCode, "passed": !t.Failed(), "at": time.Now().UTC().Format(time.RFC3339)})
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	})
}
