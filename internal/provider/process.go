// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// The provider starts a program once and keeps it running for as long as the
// provider instance itself runs (Terraform starts one per graph walk: a
// refresh, a plan, an apply). Requests go to its stdin one JSON document per line,
// each with a request_id; the program answers on stdout one JSON document per
// line, echoing the request_id, in whatever order it finishes them. Several
// requests may be in flight at once: how many the program handles at a time
// is its own business. When the provider exits the program's stdin closes
// and a conforming program exits on EOF.

// process is one running program and the requests waiting on it.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *rollingBuffer
	// readDone is closed when readLoop has stopped reading stdout; only then
	// may the process be reaped, since cmd.Wait closes the pipe.
	readDone chan struct{}
	// waited is closed once cmd.Wait has returned; exitErr holds its result.
	reapOnce sync.Once
	waited   chan struct{}
	exitErr  error

	grace time.Duration

	mu      sync.Mutex // guards pending, stale, lastAnswer, closed
	wmu     sync.Mutex // serialises writes to stdin
	pending map[string]chan result
	// lastAnswer is when the program last answered anything. A timeout
	// counts toward stale only if nothing was answered since the request
	// was written: stale means "answers have stopped coming", not "a queue
	// built up", and too many in a row means the program is stuck.
	lastAnswer time.Time
	stale      int
	closed     bool
	err        error
	// violation marks a close caused by the program breaking the protocol
	// (rather than exiting); the next request to the program reports it, so
	// that a stray print does not just restart the program silently.
	violation bool
	served    uint64
}

type result struct {
	line []byte
	err  error
}

// processes holds the live program per distinct program settings.
var processes = struct {
	sync.Mutex
	m map[string]*process
}{m: map[string]*process{}}

var requestCounter uint64

// key identifies the processes a program's requests may share: the command,
// where and with what environment it runs. Inputs travel per request.
func (p program) key() string {
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	_ = enc.Encode(p.Args)
	_ = enc.Encode(p.WorkingDir)
	_ = enc.Encode(p.InheritEnvironment)
	for _, k := range keys {
		_ = enc.Encode([2]string{k, p.Env[k]})
	}
	return b.String()
}

// process returns the live process for p, starting one if there is none or
// the previous one has ended.
func (p program) process(ctx context.Context) (*process, error) {
	processes.Lock()
	defer processes.Unlock()
	key := p.key()
	if proc, ok := processes.m[key]; ok {
		if !proc.isClosed() {
			return proc, nil
		}
		if proc.violation {
			// Report once, then start afresh on the request after.
			delete(processes.m, key)
			return nil, proc.err
		}
	}
	proc, err := p.start(ctx)
	if err != nil {
		return nil, err
	}
	processes.m[key] = proc
	return proc, nil
}

// start launches the program.
func (p program) start(ctx context.Context) (*process, error) {
	if len(p.Args) == 0 {
		return nil, errors.New("no program to run")
	}
	cmd := exec.Command(p.Args[0], p.Args[1:]...)
	cmd.Dir = p.WorkingDir
	cmd.Env = p.environment()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	proc := &process{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReaderSize(stdout, 64<<10),
		stderr:   &rollingBuffer{limit: 64 << 10},
		readDone: make(chan struct{}),
		waited:   make(chan struct{}),
		pending:  map[string]chan result{},
		grace:    p.Grace,
	}
	if proc.grace <= 0 {
		proc.grace = defaultGrace
	}
	cmd.Stderr = proc.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", p.Args[0], err)
	}
	go proc.readLoop()
	envNames := make([]string, 0, len(p.Env))
	for k := range p.Env {
		envNames = append(envNames, k)
	}
	sort.Strings(envNames)
	tflog.Debug(ctx, "scripted: started program", map[string]any{
		"args": p.Args, "working_dir": p.WorkingDir, "environment": envNames, "pid": cmd.Process.Pid,
	})
	return proc, nil
}

func (proc *process) isClosed() bool {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	return proc.closed
}

// fail ends the process and answers every waiting request with err.
func (proc *process) fail(err error) {
	proc.failWith(err, false)
}

func (proc *process) failWith(err error, violation bool) {
	proc.mu.Lock()
	if proc.closed {
		proc.mu.Unlock()
		return
	}
	proc.closed = true
	proc.err = err
	proc.violation = violation
	pending := proc.pending
	proc.pending = map[string]chan result{}
	proc.mu.Unlock()
	for _, ch := range pending {
		ch <- result{err: err}
	}
	// Stopping takes up to two grace periods and may be requested from the
	// reader itself, so it happens in the background.
	go proc.kill()
}

// reap waits for the process to exit, once, after the reader is done with
// its stdout: cmd.Wait closes the pipe, so reaping earlier would cut off
// answers still in it.
func (proc *process) reap() {
	proc.reapOnce.Do(func() {
		<-proc.readDone
		proc.exitErr = proc.cmd.Wait()
		close(proc.waited)
	})
}

// kill ends the program: close its stdin (a conforming program exits on
// EOF), then SIGTERM, then SIGKILL, with the grace period between steps.
func (proc *process) kill() {
	_ = proc.stdin.Close()
	go proc.reap()
	select {
	case <-proc.waited:
		return
	case <-time.After(proc.grace):
	}
	_ = proc.cmd.Process.Signal(sigterm)
	select {
	case <-proc.waited:
		return
	case <-time.After(proc.grace):
	}
	_ = proc.cmd.Process.Kill()
	<-proc.waited
}

// readLoop delivers each response line to the request it answers. It owns
// the stdout pipe: when the program exits, the loop sees EOF, reaps it and
// fails whatever was still pending with the exit status and stderr.
func (proc *process) readLoop() {
	for {
		line, err := readLine(proc.stdout, maxResponse)
		if err != nil && (len(bytes.TrimSpace(line)) == 0 || !errors.Is(err, io.EOF)) {
			close(proc.readDone)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				proc.reap()
				err = fmt.Errorf("the program exited (%v)\nstderr:\n%s", exitStatus(proc.exitErr), truncate(proc.stderr.String()))
			}
			proc.fail(err)
			return
		}
		// A final line without its newline, written just before the program
		// exited, is still a response; it is handled and the EOF is seen on
		// the next read.
		var head struct {
			RequestID *string `json:"request_id"`
		}
		if json.Unmarshal(line, &head) != nil || head.RequestID == nil {
			close(proc.readDone)
			proc.failWith(fmt.Errorf("the program printed a line that is not a response with a request_id; "+
				"stdout is for responses only, each echoing the request_id it answers (use stderr for messages)\nstdout:\n%s",
				truncate(string(line))), true)
			return
		}
		proc.mu.Lock()
		ch, ok := proc.pending[*head.RequestID]
		delete(proc.pending, *head.RequestID)
		// Ids are issued here, strictly increasing: an id at or below the
		// high-water mark that is no longer pending was a real request that
		// gave up waiting, however long ago. Anything else never came from
		// the provider.
		late := !ok && isIssuedID(*head.RequestID)
		if ok || late {
			proc.lastAnswer = time.Now()
			proc.stale = 0
		}
		var outstanding []string
		if !ok && !late {
			for id := range proc.pending {
				outstanding = append(outstanding, id)
			}
		}
		proc.mu.Unlock()
		switch {
		case ok:
			ch <- result{line: line}
		case late:
			// The request gave up waiting; nothing to deliver to.
		default:
			sort.Strings(outstanding)
			names := "(none)"
			if len(outstanding) > 0 {
				names = strings.Join(outstanding, ", ")
			}
			close(proc.readDone)
			proc.failWith(fmt.Errorf("the program answered request_id %q, which matches no request; outstanding: %s. "+
				"Each response must echo the request_id of the request it answers", *head.RequestID, names), true)
			return
		}
	}
}

// isIssuedID reports whether id is one the provider has handed out.
func isIssuedID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n >= 1 && n <= atomic.LoadUint64(&requestCounter)
}

// maxStale is how many requests may time out in a row, with no answer in
// between, before the program is considered stuck and stopped.
const maxStale = 3

func exitStatus(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// send writes one request and waits for its answer.
func (proc *process) send(ctx context.Context, req request, timeout time.Duration) ([]byte, error) {
	id := strconv.FormatUint(atomic.AddUint64(&requestCounter, 1), 10)
	req.RequestID = id
	stdin, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	ch := make(chan result, 1)

	proc.mu.Lock()
	if proc.closed {
		proc.mu.Unlock()
		return nil, proc.err
	}
	proc.pending[id] = ch
	proc.mu.Unlock()
	written := time.Now()
	proc.wmu.Lock()
	_, werr := proc.stdin.Write(append(stdin, '\n'))
	proc.wmu.Unlock()
	if werr != nil {
		proc.fail(fmt.Errorf("writing to the program: %w", werr))
		return nil, werr
	}
	tflog.Trace(ctx, "scripted: request", map[string]any{"stdin": string(stdin), "request_id": id})

	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case r := <-ch:
		if r.err == nil {
			atomic.AddUint64(&proc.served, 1)
		}
		return r.line, r.err
	case <-deadline:
		// Only this request gives up: the stream stays in step because every
		// answer names its request, so a late one is simply discarded. The
		// program is stopped only once answers stop coming altogether: a
		// timeout counts toward that only if nothing at all was answered
		// since this request was written, so a program working through a
		// queue one request at a time is never mistaken for a stuck one.
		proc.mu.Lock()
		delete(proc.pending, id)
		if proc.lastAnswer.Before(written) {
			proc.stale++
		} else {
			proc.stale = 0
		}
		stuck := proc.stale >= maxStale
		proc.mu.Unlock()
		if stuck {
			err := fmt.Errorf("the program did not answer within %s, nor anything else since %d requests before "+
				"it, and was stopped. A program must read one request per line and answer each; one that reads "+
				"stdin to its end waits forever", timeout, maxStale-1)
			proc.fail(err)
			return nil, err
		}
		return nil, fmt.Errorf("the program did not answer this request within %s (the time includes waiting "+
			"for the program to get to it)", timeout)
	case <-ctx.Done():
		proc.fail(errors.New("the program was interrupted"))
		return nil, errors.New("the program was interrupted")
	}
}

// run sends one request to the program and returns its parsed answer.
func (p program) run(ctx context.Context, req request) (*response, error) {
	req.Protocol = protocolVersion
	req.ProviderInput = p.ProviderInput
	req.Context = p.Context

	proc, err := p.process(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Op, err)
	}
	ctx = tflog.SetField(ctx, "op", req.Op)
	ctx = tflog.SetField(ctx, "pid", proc.cmd.Process.Pid)
	started := time.Now()
	line, err := proc.send(ctx, req, p.effectiveTimeout())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Op, err)
	}
	tflog.Trace(ctx, "scripted: response", map[string]any{"stdout": string(line)})

	resp := &response{}
	if err := parseResponse(line, resp); err != nil {
		return nil, fmt.Errorf("%s: %w\nstdout:\n%s", req.Op, err, truncate(string(line)))
	}
	fields := map[string]any{
		// elapsed is send-to-answer; processing, when the program reports
		// it, is its own time on the request. The difference is queueing
		// plus transport and, for a program's first request, its start-up,
		// so it is left to the reader rather than labelled.
		"elapsed": time.Since(started).String(),
		"served":  atomic.LoadUint64(&proc.served),
		// stderr is the program's, not the request's: with requests handled
		// concurrently this is whatever was written since the last answer.
		"stderr_since_last_answer": proc.stderr.TakeString(),
	}
	if resp.DurationMS != nil {
		fields["processing"] = processingDuration(*resp.DurationMS).String()
	}
	tflog.Debug(ctx, "scripted: program answered", fields)
	if resp.Protocol != nil && *resp.Protocol > protocolVersion {
		return nil, fmt.Errorf("%s: the program declares protocol %d but this provider speaks protocol %d; upgrade the provider",
			req.Op, *resp.Protocol, protocolVersion)
	}
	tflog.Debug(ctx, "scripted: program response", map[string]any{
		"not_implemented":  resp.NotImplemented,
		"id":               resp.ID,
		"has_output":       resp.Output != nil,
		"has_input":        resp.Input != nil,
		"exists":           resp.Exists,
		"requires_replace": resp.RequiresReplace,
		"warnings":         len(resp.Warnings),
		"errors":           len(resp.Errors),
		"has_private":      resp.Private != nil,
	})
	return resp, nil
}

// readLine reads up to and including the next newline, refusing lines
// longer than limit.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, fmt.Errorf("the program printed a response longer than %d bytes", limit)
		}
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return out, err
		}
	}
}

// rollingBuffer keeps the most recent limit bytes written to it, safely
// across goroutines (the program writes stderr while the provider reads it).
type rollingBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *rollingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append([]byte("..."), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *rollingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// TakeString returns what was written since the last call and clears it.
func (b *rollingBuffer) TakeString() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	b.buf = b.buf[:0]
	return s
}

// processingDuration converts a program's duration_ms, which may be
// fractional, without losing the fraction.
func processingDuration(ms float64) time.Duration {
	return time.Duration(ms * float64(time.Millisecond))
}
