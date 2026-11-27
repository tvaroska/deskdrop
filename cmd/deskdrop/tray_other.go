//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

func defaultShowsTrayOnNoArgs() bool {
	return false
}

func runPlatformTray(cfgPath string, cdpURL string, interval time.Duration, fallbackGate string, _ bool) error {
	if interval <= 0 {
		interval = 20 * time.Second
	}
	args := []string{"--interval", interval.String()}
	if cfgPath != "" {
		args = append(args, "--config", cfgPath)
	}
	if cdpURL != "" {
		args = append(args, "--cdp", cdpURL)
	}
	if fallbackGate != "" {
		args = append(args, "--fallback-gate", fallbackGate)
	}
	fmt.Printf("Running persistent deskdrop worker (interval=%s)...\n", interval)
	return cmdSync(args)
}

// confirmFallbackPlan asks for local confirmation when --fallback-gate=tray on non-Windows.
// Fails closed if stdin is not an interactive terminal.
func confirmFallbackPlan(plan contract.FallbackPlan) (bool, error) {
	return confirmFallbackPlanWithTimeout(context.Background(), defaultFallbackPromptTimeout, plan)
}

func confirmFallbackPlanWithTimeout(ctx context.Context, timeout time.Duration, plan contract.FallbackPlan) (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return false, nil
	}
	return promptFallbackFromFile(ctx, timeout, os.Stdin, os.Stderr, plan)
}

type stdinLineResult struct {
	line string
	err  error
}

type nonPollableLineReader struct {
	file     *os.File
	mu       sync.Mutex
	activeCh chan stdinLineResult
	reading  bool
	wakeCh   chan struct{}
}

var nonPollableReaders sync.Map // map[*os.File]*nonPollableLineReader

func getNonPollableReader(inFile *os.File) *nonPollableLineReader {
	if existing, ok := nonPollableReaders.Load(inFile); ok {
		return existing.(*nonPollableLineReader)
	}
	r := &nonPollableLineReader{
		file:   inFile,
		wakeCh: make(chan struct{}, 1),
	}
	actual, loaded := nonPollableReaders.LoadOrStore(inFile, r)
	if loaded {
		return actual.(*nonPollableLineReader)
	}
	go r.loop()
	return r
}

func (r *nonPollableLineReader) requestLine() chan stdinLineResult {
	ch := make(chan stdinLineResult, 1)
	r.mu.Lock()
	r.activeCh = ch
	needWake := !r.reading
	if needWake {
		r.reading = true
	}
	r.mu.Unlock()
	if needWake {
		r.wakeCh <- struct{}{}
	}
	return ch
}

func (r *nonPollableLineReader) deliver(line string, err error) {
	r.mu.Lock()
	ch := r.activeCh
	r.activeCh = nil
	r.reading = false
	r.mu.Unlock()
	if ch != nil {
		ch <- stdinLineResult{line: line, err: err}
	}
}

func (r *nonPollableLineReader) clearActive(ch chan stdinLineResult) {
	r.mu.Lock()
	if r.activeCh == ch {
		r.activeCh = nil
	}
	r.mu.Unlock()
}

func (r *nonPollableLineReader) loop() {
	var one [1]byte
	for range r.wakeCh {
		var buf []byte
		for {
			n, err := r.file.Read(one[:])
			if n > 0 {
				if len(buf) < 256 {
					buf = append(buf, one[0])
				}
				if one[0] == '\n' {
					r.deliver(string(buf), nil)
					break
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) && len(buf) > 0 {
					r.deliver(string(buf), nil)
				} else {
					r.deliver("", err)
				}
				nonPollableReaders.Delete(r.file)
				return
			}
		}
	}
}

// promptFallbackFromFile writes the Tier-2 approval prompt and reads one response line within timeout
// without leaking per-prompt blocked reader goroutines or stealing bytes across sequential prompts.
func promptFallbackFromFile(
	parentCtx context.Context,
	timeout time.Duration,
	inFile *os.File,
	out io.Writer,
	plan contract.FallbackPlan,
) (bool, error) {
	if timeout <= 0 {
		timeout = defaultFallbackPromptTimeout
	}
	pctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if err := parentCtx.Err(); err != nil {
		return false, err
	}

	fmt.Fprintf(out, "\n[DeskDrop Tier-2 Fallback Approval]\nDomain: %s\nURL: %s\nReason: %s\nActions (%d):\n",
		plan.AllowedDomain, plan.TargetURL, plan.Reason, len(plan.Actions))
	for i, a := range plan.Actions {
		fmt.Fprintf(out, "  %d. %s selector=%q delta_y=%d wait_ms=%d\n", i+1, a.Action, a.Selector, a.DeltaY, a.WaitMs)
	}
	fmt.Fprint(out, "Allow bounded stateless fallback? [y/N]: ")

	line, err := readLineBounded(pctx, timeout, parentCtx, inFile)
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func readLineBounded(pctx context.Context, timeout time.Duration, parentCtx context.Context, inFile *os.File) (string, error) {
	// Fast path: pollable os.File (pipes, sockets, epoll-registered fds) runs synchronously on the
	// current goroutine using short read deadlines — zero child goroutines spawned.
	probeDeadline := time.Now().Add(20 * time.Millisecond)
	if err := inFile.SetReadDeadline(probeDeadline); err == nil {
		defer func() { _ = inFile.SetReadDeadline(time.Time{}) }()
		var buf []byte
		var one [1]byte
		for {
			if err := pctx.Err(); err != nil {
				if parentCtx.Err() != nil {
					return "", parentCtx.Err()
				}
				return "", fmt.Errorf("approval prompt timed out after %s", timeout)
			}
			stepDeadline := time.Now().Add(25 * time.Millisecond)
			if dl, ok := pctx.Deadline(); ok && dl.Before(stepDeadline) {
				stepDeadline = dl
			}
			_ = inFile.SetReadDeadline(stepDeadline)
			n, rerr := inFile.Read(one[:])
			if n > 0 {
				if len(buf) < 256 {
					buf = append(buf, one[0])
				}
				if one[0] == '\n' {
					return string(buf), nil
				}
			}
			if rerr != nil {
				if errors.Is(rerr, os.ErrDeadlineExceeded) {
					continue
				}
				if errors.Is(rerr, io.EOF) && len(buf) > 0 {
					return string(buf), nil
				}
				return "", rerr
			}
		}
	}

	// Fallback for non-pollable character TTYs (such as os.Stdin constructed via os.NewFile(0, ...)):
	// use a single demand-driven 1-byte reader per *os.File so timed-out prompts never leak extra
	// goroutines, never buffer ahead across sequential prompts, and discard lines typed while idle.
	r := getNonPollableReader(inFile)
	ch := r.requestLine()

	select {
	case res := <-ch:
		if res.err != nil && (res.line == "" || !errors.Is(res.err, io.EOF)) {
			return "", res.err
		}
		return res.line, nil
	case <-pctx.Done():
		r.clearActive(ch)
		if parentCtx.Err() != nil {
			return "", parentCtx.Err()
		}
		return "", fmt.Errorf("approval prompt timed out after %s", timeout)
	}
}
