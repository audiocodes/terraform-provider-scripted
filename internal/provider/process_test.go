// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pyProgram builds a program that runs the given Python source. Each
// distinct source is a distinct program, so tests do not share processes.
func pyProgram(t *testing.T, timeout time.Duration, source string) program {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	// A per-call marker in the environment gives every test run its own
	// process, so repeated runs (-count) never share state.
	runMarker := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	return program{Args: []string{"python3", "-c", source}, InheritEnvironment: true, Timeout: timeout,
		Grace: 200 * time.Millisecond, Env: map[string]string{"SCRIPTED_TEST_RUN": runMarker}}
}

// A correct final response written without a trailing newline, just before
// the program exits, is still delivered.
func TestProgram_unterminatedFinalLine(t *testing.T) {
	p := pyProgram(t, 5*time.Second, `
import json, sys
line = sys.stdin.readline()
req = json.loads(line)
sys.stdout.write(json.dumps({"request_id": req["request_id"], "output": "last words"}))
sys.stdout.flush()
`)
	resp, err := p.run(context.Background(), request{Op: "read"})
	if err != nil || resp == nil || string(resp.Output) != `"last words"` {
		t.Fatalf("expected the unterminated final response to be delivered: %v %v", err, resp)
	}
}

// A program that writes a burst of answers and exits at once must have every
// one of them delivered. The reader goroutine owns stdout and reaps the
// process only after the pipe has drained; if waiting on the process raced
// the reader, cmd.Wait would close the pipe under it and the answers still
// in flight would be lost to "the program exited". Bulky answers keep the
// pipe full when the program exits, which is when that race bites.
func TestProgram_answersDeliveredWhenProgramExitsAtOnce(t *testing.T) {
	const n = 200
	for round := 0; round < 5; round++ {
		p := pyProgram(t, 10*time.Second, `
import json, os, sys
reqs = [json.loads(sys.stdin.readline()) for _ in range(int(os.environ["SCRIPTED_TEST_N"]))]
for req in reqs:
    sys.stdout.write(json.dumps({"request_id": req["request_id"], "output": {"n": req["input"], "pad": "x" * 4096}}) + "\n")
sys.stdout.flush()
os._exit(0)
`)
		p.Env["SCRIPTED_TEST_N"] = fmt.Sprint(n)
		errs := make(chan error, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				resp, err := p.run(context.Background(), request{Op: "read", Input: i})
				switch {
				case err != nil:
					errs <- fmt.Errorf("request %d: %w", i, err)
				case resp == nil || !strings.Contains(string(resp.Output), fmt.Sprintf(`"n": %d,`, i)):
					errs <- fmt.Errorf("request %d: wrong answer %.80s", i, resp.Output)
				default:
					errs <- nil
				}
			}(i)
		}
		for i := 0; i < n; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: an answer written just before the program exited was not delivered: %v", round, err)
			}
		}
	}
}

// loopProgram answers every line with the given Python expression for the
// response dict (req is the decoded request).
func loopProgram(t *testing.T, respExpr string) program {
	t.Helper()
	return pyProgram(t, 5*time.Second, fmt.Sprintf(`
import json, sys
for line in sys.stdin:
    req = json.loads(line)
    print(json.dumps(%s), flush=True)
`, respExpr))
}

func TestProgram_answersAndReusesProcess(t *testing.T) {
	p := loopProgram(t, `{"request_id": req["request_id"], "output": {"op": req["op"], "n": req["input"]}}`)
	ctx := context.Background()
	var pid int
	for i := 0; i < 3; i++ {
		resp, err := p.run(ctx, request{Op: "read", Input: i})
		if err != nil || resp == nil || string(resp.Output) != fmt.Sprintf(`{"op": "read", "n": %d}`, i) {
			t.Fatalf("run %d: %v %s", i, err, resp.Output)
		}
		proc, _ := p.process(ctx)
		if pid == 0 {
			pid = proc.cmd.Process.Pid
		} else if proc.cmd.Process.Pid != pid {
			t.Fatalf("expected the same process, got pid %d then %d", pid, proc.cmd.Process.Pid)
		}
	}
}

func TestProgram_exitsWithoutAnswering(t *testing.T) {
	p := pyProgram(t, 5*time.Second, `import sys; sys.stderr.write("boom\n"); sys.exit(3)`)
	_, err := p.run(context.Background(), request{Op: "create"})
	if err == nil || !strings.Contains(err.Error(), "exited (exit status 3)") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected exit status and stderr in the error, got: %v", err)
	}
	// The next request starts a fresh process and fails the same way.
	if _, err := p.run(context.Background(), request{Op: "read"}); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("restart: %v", err)
	}
}

func TestProgram_strayOutputFailsNextRequestOnce(t *testing.T) {
	p := pyProgram(t, 5*time.Second, `
import json, sys
first = True
for line in sys.stdin:
    req = json.loads(line)
    print(json.dumps({"request_id": req["request_id"]}), flush=True)
    if first:
        print("debug: all good", flush=True)
        first = False
`)
	ctx := context.Background()
	if _, err := p.run(ctx, request{Op: "read"}); err != nil {
		t.Fatalf("first request should succeed: %v", err)
	}
	// The stray line arrives after the answer; give the reader a moment to
	// see it and end the process.
	for i := 0; i < 50; i++ {
		processes.Lock()
		proc := processes.m[p.key()]
		processes.Unlock()
		if proc == nil || proc.isClosed() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The stray line ended the process; the next request reports it...
	_, err := p.run(ctx, request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "not a response with a request_id") || !strings.Contains(err.Error(), "debug: all good") {
		t.Fatalf("expected the protocol violation to be reported: %v", err)
	}
	// ...and the one after that starts afresh.
	if _, err := p.run(ctx, request{Op: "read"}); err != nil {
		t.Fatalf("third request should restart the program: %v", err)
	}
}

// A request that times out is given up on its own; only when answers stop
// coming altogether is the program stopped, and a stubborn one (ignores EOF
// and SIGTERM) has to be killed.
func TestProgram_timeoutsThenKillStubbornProcess(t *testing.T) {
	p := pyProgram(t, 300*time.Millisecond, `
import signal, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
for line in sys.stdin:
    pass
time.sleep(60)
`)
	ctx := context.Background()
	for i := 1; i < maxStale; i++ {
		_, err := p.run(ctx, request{Op: "read"})
		if err == nil || !strings.Contains(err.Error(), "did not answer this request within 300ms") || strings.Contains(err.Error(), "stopped") {
			t.Fatalf("timeout %d should fail only its own request: %v", i, err)
		}
	}
	started := time.Now()
	_, err := p.run(ctx, request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "was stopped") {
		t.Fatalf("timeout %d should stop the program: %v", maxStale, err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("stopping took %s; SIGKILL fallback did not fire", took)
	}
}

// A slow request does not hold up or fail the others on the same program.
func TestProgram_slowRequestDoesNotFailOthers(t *testing.T) {
	p := pyProgram(t, 400*time.Millisecond, `
import json, sys, threading, time
def answer(req):
    if req["input"] == "slow":
        time.sleep(2)
    print(json.dumps({"request_id": req["request_id"], "output": req["input"]}), flush=True)
for line in sys.stdin:
    threading.Thread(target=answer, args=(json.loads(line),)).start()
`)
	ctx := context.Background()
	errs := make(chan error, 3)
	for _, in := range []string{"slow", "fast-1", "fast-2"} {
		go func(in string) {
			resp, err := p.run(ctx, request{Op: "read", Input: in})
			if err == nil && string(resp.Output) != fmt.Sprintf("%q", in) {
				err = fmt.Errorf("wrong answer for %s: %s", in, resp.Output)
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", in, err)
			}
			errs <- err
		}(in)
	}
	var failed []string
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) != 1 || !strings.HasPrefix(failed[0], "slow:") || !strings.Contains(failed[0], "did not answer this request") {
		t.Fatalf("only the slow request should fail, with a timeout: %v", failed)
	}
	// The program is still alive and answering.
	if _, err := p.run(ctx, request{Op: "read", Input: "after"}); err != nil {
		t.Fatalf("program should still be usable: %v", err)
	}
}

// Echoing the wrong request_id is reported as such, naming the ids, rather
// than looking like a program that never answered.
func TestProgram_wrongRequestIDIsAViolation(t *testing.T) {
	// An id far above anything the provider has issued.
	p := loopProgram(t, `{"request_id": req["request_id"] + "000000000"}`)
	_, err := p.run(context.Background(), request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "matches no request") || !strings.Contains(err.Error(), "outstanding: ") {
		t.Fatalf("expected a request_id violation naming the ids: %v", err)
	}
}

// A late answer to a request that already timed out is not a protocol
// violation, however late it is: the id was one the provider issued.
func TestProgram_lateAnswerIsNotAViolation(t *testing.T) {
	p := pyProgram(t, 300*time.Millisecond, `
import json, sys, time
first = True
for line in sys.stdin:
    req = json.loads(line)
    if first:
        time.sleep(1.2)
        first = False
    print(json.dumps({"request_id": req["request_id"]}), flush=True)
`)
	ctx := context.Background()
	if _, err := p.run(ctx, request{Op: "read"}); err == nil || !strings.Contains(err.Error(), "did not answer this request") {
		t.Fatalf("first request should time out: %v", err)
	}
	// Well past 2x the timeout, the late answer arrives while the program is
	// idle; then the program must still be alive and in good standing.
	time.Sleep(1500 * time.Millisecond)
	if _, err := p.run(ctx, request{Op: "read"}); err != nil {
		t.Fatalf("late answer should be discarded, not treated as a violation: %v", err)
	}
}

// The helper answers every line it is given, even one that is not a request.
func TestHelper_alwaysAnswers(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
	wd, _ := os.Getwd()
	cmd := exec.Command("python3", "-c", fmt.Sprintf(`
import sys
sys.path.insert(0, %q)
from scripted import resource
r = resource()
@r.read
def read(req):
    raise RuntimeError("kaboom")
r.main()
`, filepath.Join(wd, "..", "..", "helpers", "python")))
	// Not an object, malformed JSON, and a handler that raises: three
	// different failure paths, each must still produce exactly one answer.
	cmd.Stdin = strings.NewReader("5\n{\n" + `{"request_id": "9", "op": "read"}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper exited badly: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected one answer per line, got %d: %s", len(lines), out)
	}
	joined := string(out)
	if !strings.Contains(joined, "not a JSON object") || !strings.Contains(joined, "JSONDecodeError") ||
		!strings.Contains(joined, "kaboom") || !strings.Contains(joined, `"request_id": "9"`) {
		t.Fatalf("answers should carry the errors and the request_id: %s", out)
	}
	if !strings.Contains(joined, `"duration_ms": `) {
		t.Fatalf("the handler's answer should report duration_ms: %s", out)
	}
}

func TestIsIssuedID(t *testing.T) {
	atomic.AddUint64(&requestCounter, 1)
	if !isIssuedID("1") || isIssuedID("0") || isIssuedID("x") || isIssuedID(fmt.Sprint(atomic.LoadUint64(&requestCounter)+1)) {
		t.Error("isIssuedID")
	}
}

// An unconfigured program is bounded: the default is the documented ten
// minutes, and it applies whenever nothing else is set.
func TestProgram_defaultTimeout(t *testing.T) {
	t.Parallel()
	if defaultTimeout != 10*time.Minute {
		t.Fatalf("documented default is 10 minutes, got %s", defaultTimeout)
	}
	if got := (program{}).effectiveTimeout(); got <= 0 || got != defaultTimeout {
		t.Errorf("unconfigured program must be bounded by the default, got %s", got)
	}
	if got := (program{Timeout: time.Second}).effectiveTimeout(); got != time.Second {
		t.Errorf("explicit timeout wins, got %s", got)
	}
}

func TestProcessingDuration(t *testing.T) {
	t.Parallel()
	if got := processingDuration(0.4); got != 400*time.Microsecond {
		t.Errorf("fractional milliseconds must survive, got %s", got)
	}
	if got := processingDuration(1500); got != 1500*time.Millisecond {
		t.Errorf("got %s", got)
	}
}

func TestProgram_newerProtocolRefused(t *testing.T) {
	p := loopProgram(t, `{"request_id": req["request_id"], "protocol": 2}`)
	_, err := p.run(context.Background(), request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "declares protocol 2") {
		t.Fatalf("expected refusal: %v", err)
	}
}

func TestProgram_interrupted(t *testing.T) {
	p := pyProgram(t, 0, `
import sys, time
for line in sys.stdin:
    time.sleep(60)
`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := p.run(ctx, request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("expected interruption: %v", err)
	}
}

func TestProgram_invalidJSONLineWithRequestIDText(t *testing.T) {
	// Looks like it has a request_id but is not valid JSON.
	p := loopProgram(t, `"{request_id: " + req["request_id"] + "}"`)
	// The expression above yields a JSON string, i.e. a line like
	// "\"{request_id: 1}\"", which decodes to a string, not an object.
	_, err := p.run(context.Background(), request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "not a response with a request_id") {
		t.Fatalf("expected a protocol violation: %v", err)
	}
}

func TestProgram_startFailures(t *testing.T) {
	ctx := context.Background()
	if _, err := (program{}).run(ctx, request{Op: "read"}); err == nil || !strings.Contains(err.Error(), "no program to run") {
		t.Errorf("empty program: %v", err)
	}
	if _, err := (program{Args: []string{"/nonexistent/program"}}).run(ctx, request{Op: "read"}); err == nil || !strings.Contains(err.Error(), `starting "/nonexistent/program"`) {
		t.Errorf("missing program: %v", err)
	}
	if _, err := (program{Args: []string{"python3", "-c", "pass"}, WorkingDir: "/nonexistent/dir"}).run(ctx, request{Op: "read"}); err == nil {
		t.Errorf("bad working dir should fail")
	}
}

func TestProgram_longResponseLine(t *testing.T) {
	p := loopProgram(t, `{"request_id": req["request_id"], "output": "x" * (maxResponse + 10)}`)
	p.Args[2] = strings.Replace(p.Args[2], "maxResponse", fmt.Sprint(maxResponse), 1)
	_, err := p.run(context.Background(), request{Op: "read"})
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("expected the response size limit: %v", err)
	}
}

// A program that works through its queue one request at a time, slower than
// the timeout per request, is not stuck: requests waiting their turn may time
// out, but the program keeps answering and is never stopped.
func TestProgram_sequentialQueueIsNotStuck(t *testing.T) {
	p := pyProgram(t, 500*time.Millisecond, `
import json, sys, time
for line in sys.stdin:
    req = json.loads(line)
    time.sleep(0.3)
    print(json.dumps({"request_id": req["request_id"]}), flush=True)
`)
	ctx := context.Background()
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			_, err := p.run(ctx, request{Op: "read"})
			errs <- err
		}()
	}
	timeouts := 0
	for i := 0; i < 6; i++ {
		err := <-errs
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "was stopped") {
			t.Fatalf("a healthy sequential program was declared stuck: %v", err)
		}
		if !strings.Contains(err.Error(), "did not answer this request") {
			t.Fatalf("unexpected error: %v", err)
		}
		timeouts++
	}
	if timeouts == 0 {
		t.Fatal("expected some queued requests to time out (the test would otherwise prove nothing)")
	}
	// Still alive and answering once the queue has drained.
	time.Sleep(2 * time.Second)
	if _, err := p.run(ctx, request{Op: "read"}); err != nil {
		t.Fatalf("program should still be usable: %v", err)
	}
}

// r.local() builds one client per worker thread, not per request.
func TestHelper_localIsPerWorker(t *testing.T) {
	wd, _ := os.Getwd()
	helperDir := filepath.Join(wd, "..", "..", "helpers", "python")
	p := pyProgram(t, 5*time.Second, fmt.Sprintf(`
import sys, threading
sys.path.insert(0, %q)
from scripted import resource
r = resource(concurrent=4)
built = []
lock = threading.Lock()
def factory():
    with lock:
        built.append(1)
    return object()
client = r.local(factory)
@r.read
def read(req):
    client()
    return r.ok(output={"built": len(built)})
r.main()
`, helperDir))
	ctx := context.Background()
	var last *response
	for i := 0; i < 40; i++ {
		resp, err := p.run(ctx, request{Op: "read"})
		if err != nil {
			t.Fatal(err)
		}
		last = resp
	}
	var out struct {
		Built int `json:"built"`
	}
	if err := json.Unmarshal(last.Output, &out); err != nil {
		t.Fatal(err)
	}
	if out.Built < 1 || out.Built > 4 {
		t.Fatalf("expected at most 4 clients (one per worker) for 40 requests, got %d", out.Built)
	}
}
