//go:build !windows

package main

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tvaroska/deskdrop/internal/contract"
)

type notifyPromptWriter struct {
	buf      bytes.Buffer
	promptCh chan struct{}
}

func (w *notifyPromptWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if strings.Contains(string(p), "[y/N]:") {
		select {
		case w.promptCh <- struct{}{}:
		default:
		}
	}
	return n, err
}

func TestM812PromptFallbackFromFilePollableAndNonPollableNoLeakOrStaleAccept(t *testing.T) {
	plan := contract.FallbackPlan{
		AllowedDomain: "status.example.com",
		TargetURL:     "https://status.example.com",
		Reason:        "Dismiss modal",
		Actions:       []contract.FallbackAction{{Action: "click", Selector: "button.ok"}},
	}

	// 1. Pollable os.Pipe() fast path (SetReadDeadline supported: 0 spawned goroutines).
	rPipe, wPipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rPipe.Close()
	defer wPipe.Close()

	var outBuf strings.Builder
	baseG := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		ok, perr := promptFallbackFromFile(context.Background(), 25*time.Millisecond, rPipe, &outBuf, plan)
		if ok || perr == nil || !strings.Contains(perr.Error(), "timed out") {
			t.Fatalf("pollable iter %d = (%v, %v), want timeout", i, ok, perr)
		}
	}
	if _, err := wPipe.Write([]byte("y\nno\n")); err != nil {
		t.Fatal(err)
	}
	if ok, err := promptFallbackFromFile(context.Background(), time.Second, rPipe, &outBuf, plan); err != nil || !ok {
		t.Fatalf("pollable 'y' = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := promptFallbackFromFile(context.Background(), time.Second, rPipe, &outBuf, plan); err != nil || ok {
		t.Fatalf("pollable 'no' = (%v, %v), want (false, nil)", ok, err)
	}

	// 2. Non-pollable os.NewFile(dupFd, ...) path (exact same kindNewFile / ErrNoDeadline behavior as os.Stdin).
	rRaw, wRaw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rRaw.Close()
	dupFD, err := syscall.Dup(int(rRaw.Fd()))
	if err != nil {
		wRaw.Close()
		t.Fatal(err)
	}
	nonPollIn := os.NewFile(uintptr(dupFD), "nonpollable-stdin")
	defer nonPollIn.Close()

	if derr := nonPollIn.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); derr == nil {
		wRaw.Close()
		t.Fatal("expected os.NewFile(dupFD) to be non-pollable like os.Stdin")
	}

	pw := &notifyPromptWriter{promptCh: make(chan struct{}, 1)}

	// 4 consecutive timeouts on non-pollable fd must spawn at most 1 shared reader goroutine.
	for i := 0; i < 4; i++ {
		ok, perr := promptFallbackFromFile(context.Background(), 25*time.Millisecond, nonPollIn, pw, plan)
		if ok || perr == nil || !strings.Contains(perr.Error(), "timed out") {
			wRaw.Close()
			t.Fatalf("non-pollable timeout %d = (%v, %v), want timeout", i, ok, perr)
		}
		select {
		case <-pw.promptCh:
		default:
		}
	}
	if after := runtime.NumGoroutine(); after > baseG+2 {
		wRaw.Close()
		t.Fatalf("goroutine leak on non-pollable timeouts: base=%d after=%d", baseG, after)
	}

	// Fresh user answers written AFTER each prompt renders succeed deterministically without byte-stealing.
	go func() {
		<-pw.promptCh
		_, _ = wRaw.Write([]byte("yes\n"))
	}()
	if ok, err := promptFallbackFromFile(context.Background(), 2*time.Second, nonPollIn, pw, plan); err != nil || !ok {
		wRaw.Close()
		t.Fatalf("non-pollable 'yes' = (%v, %v), want (true, nil)", ok, err)
	}

	go func() {
		<-pw.promptCh
		_, _ = wRaw.Write([]byte("n\n"))
	}()
	if ok, err := promptFallbackFromFile(context.Background(), 2*time.Second, nonPollIn, pw, plan); err != nil || ok {
		wRaw.Close()
		t.Fatalf("non-pollable 'n' = (%v, %v), want (false, nil)", ok, err)
	}
	_ = wRaw.Close()
}
