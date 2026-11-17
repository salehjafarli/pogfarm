//go:build unix

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// helperEnv selects a helper mode. When it is set, the test binary acts as a
// small external program instead of running tests, so the tests need nothing
// beyond sh and sleep on the machine.
const helperEnv = "RUNNER_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	switch mode {
	case "lines":
		// 5000 numbered lines on each stream.
		for i := 1; i <= 5000; i++ {
			fmt.Fprintf(os.Stdout, "out %d\n", i)
			fmt.Fprintf(os.Stderr, "err %d\n", i)
		}
		return 0
	case "flood":
		// Writes until killed.
		block := bytes.Repeat([]byte("0123456789abcdef\n"), 1024)
		for {
			if _, err := os.Stdout.Write(block); err != nil {
				return 1
			}
		}
	case "escape":
		// Starts a grandchild in its own session, outside the process
		// group, that keeps stdout and stderr open; then exits.
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), helperEnv+"=sleep")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(cmd.Process.Pid)
		return 0
	case "sleep":
		time.Sleep(20 * time.Second)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown helper mode", mode)
	return 2
}

// helper returns a Command that runs the test binary in the given mode.
func helper(mode string) Command {
	return Command{Name: os.Args[0], Env: []string{helperEnv + "=" + mode}}
}

// alive reports whether a process with this pid exists.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) != syscall.ESRCH
}

// waitGone fails the test unless the process disappears. A killed orphan
// stays visible for a moment until init reaps it, hence the polling.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parsePid reads the pid a script printed as its first line of stdout.
func parsePid(t *testing.T, stdout []byte) int {
	t.Helper()
	first, _, _ := strings.Cut(string(stdout), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || pid <= 0 {
		t.Fatalf("no child pid in stdout %q", stdout)
	}
	return pid
}

// asError fails the test unless err is a *Error.
func asError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T (%v), want *Error", err, err)
	}
	return e
}

// spawnChild is a script that starts a long-lived child, prints the child's
// pid and then waits for it.
const spawnChild = `sleep 300 & echo $!; wait`

func TestRunReturnsOutputExitCodeAndDuration(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Command{
		Name: "sh",
		Args: []string{"-c", "echo to-stdout; echo to-stderr >&2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "to-stdout\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := string(res.Stderr); got != "to-stderr\n" {
		t.Errorf("stderr = %q", got)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.StdoutTruncated || res.StderrTruncated {
		t.Error("short output reported as truncated")
	}
	if res.Duration <= 0 {
		t.Errorf("duration = %v, want > 0", res.Duration)
	}
}

func TestRunDoesNotUseAShell(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Command{
		Name: "echo",
		Args: []string{"a  b", "$HOME", ";", "exit", "1", "`id`", "*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(res.Stdout), "a  b $HOME ; exit 1 `id` *\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunNonZeroExit(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Command{
		Name: "sh",
		Args: []string{"-c", "echo partial; echo first problem >&2; echo second problem >&2; exit 3"},
	})
	e := asError(t, err)
	if e.Name != "sh" {
		t.Errorf("Name = %q, want sh", e.Name)
	}
	if e.ExitCode != 3 || res.ExitCode != 3 {
		t.Errorf("exit code: error %d, result %d, want 3", e.ExitCode, res.ExitCode)
	}
	if e.Stderr != "first problem\nsecond problem" {
		t.Errorf("Stderr = %q", e.Stderr)
	}
	if e.Err != nil {
		t.Errorf("Err = %v, want nil for a plain non-zero exit", e.Err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Error("a plain failure matches a context error")
	}
	if got, want := err.Error(), "run sh: exit code 3; stderr tail:\nfirst problem\nsecond problem"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	// The output captured before the failure is still returned.
	if got := string(res.Stdout); got != "partial\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestRunErrorKeepsLastLinesOfStderr(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Command{
		Name: "sh",
		Args: []string{"-c", `i=1; while [ $i -le 50 ]; do echo "line $i" >&2; i=$((i+1)); done; exit 1`},
	})
	e := asError(t, err)
	lines := strings.Split(e.Stderr, "\n")
	if len(lines) != errTailLines {
		t.Fatalf("kept %d lines, want %d", len(lines), errTailLines)
	}
	if lines[0] != "line 31" || lines[len(lines)-1] != "line 50" {
		t.Errorf("kept %q .. %q, want line 31 .. line 50", lines[0], lines[len(lines)-1])
	}
}

func TestRunCommandNotFound(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Command{Name: "pogfarm-no-such-tool"})
	e := asError(t, err)
	if e.ExitCode != -1 || res.ExitCode != -1 {
		t.Errorf("exit code: error %d, result %d, want -1", e.ExitCode, res.ExitCode)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error %v does not wrap exec.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "pogfarm-no-such-tool") {
		t.Errorf("message %q lacks the command name", err)
	}
}

func TestRunKilledBySignal(t *testing.T) {
	t.Parallel()
	res, err := Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "kill -KILL $$"}})
	e := asError(t, err)
	if e.ExitCode != -1 || res.ExitCode != -1 {
		t.Errorf("exit code: error %d, result %d, want -1", e.ExitCode, res.ExitCode)
	}
	if e.Err == nil {
		t.Error("Err is nil for a process killed by a signal")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Error("a signal death matches a context error")
	}
}

func TestRunTimeoutKillsProcessAndChild(t *testing.T) {
	t.Parallel()
	// The timeout has to outlast the script's startup, which takes longer
	// than a second on a heavily loaded machine. When the kill arrives
	// before the script has written anything, the run says nothing about
	// the behaviour under test, so it is repeated with a longer timeout.
	timeouts := []time.Duration{time.Second, 5 * time.Second, 20 * time.Second}
	for i, timeout := range timeouts {
		start := time.Now()
		res, err := Run(context.Background(), Command{
			Name:    "sh",
			Args:    []string{"-c", "echo dying >&2; " + spawnChild},
			Timeout: timeout,
		})
		elapsed := time.Since(start)

		e := asError(t, err)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
		}
		if e.ExitCode != -1 || res.ExitCode != -1 {
			t.Errorf("exit code: error %d, result %d, want -1", e.ExitCode, res.ExitCode)
		}
		if elapsed < timeout || elapsed > timeout+30*time.Second {
			t.Errorf("returned after %v, want shortly after the %v timeout", elapsed, timeout)
		}
		if len(res.Stdout) == 0 && i < len(timeouts)-1 {
			t.Logf("script had not reported its child within %v; retrying with a longer timeout", timeout)
			continue
		}

		if e.Stderr != "dying" {
			t.Errorf("Stderr = %q, want the output from before the kill", e.Stderr)
		}
		if got, want := err.Error(), "run sh: timed out and was killed; stderr tail:\ndying"; got != want {
			t.Errorf("message = %q, want %q", got, want)
		}
		waitGone(t, parsePid(t, res.Stdout))
		return
	}
}

func TestRunCancelKillsProcessAndChild(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, Command{
		Name: "sh",
		Args: []string{"-c", spawnChild},
		// Cancel once the script has reported its child, so the child is
		// known to exist when the kill happens.
		OnLine: func(Stream, string) { cancel() },
	})
	elapsed := time.Since(start)

	e := asError(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
	if e.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", e.ExitCode)
	}
	if !strings.Contains(err.Error(), "run sh: cancelled") {
		t.Errorf("message = %q", err)
	}
	if elapsed > 30*time.Second {
		t.Errorf("returned after %v, want promptly after the cancel", elapsed)
	}
	waitGone(t, parsePid(t, res.Stdout))
}

func TestRunContextAlreadyDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "started")
	_, err := Run(ctx, Command{Name: "sh", Args: []string{"-c", `echo x > "$1"`, "sh", marker}})
	asError(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the command ran although the context was already cancelled")
	}
}

func TestRunParentDeadlineCountsAsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, Command{Name: "sleep", Args: []string{"300"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v does not wrap context.DeadlineExceeded", err)
	}
}

func TestRunKillsChildLeftBehindAfterExit(t *testing.T) {
	t.Parallel()
	start := time.Now()
	res, err := Run(context.Background(), Command{
		Name: "sh",
		// The script exits at once; its child keeps the output pipes open.
		Args: []string{"-c", "sleep 300 & echo $!"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("returned after %v; Run waited for the background child", elapsed)
	}
	waitGone(t, parsePid(t, res.Stdout))
}

func TestRunDirAndEnv(t *testing.T) {
	// Not parallel: t.Setenv.
	t.Setenv("RUNNER_TEST_INHERITED", "inherited")
	t.Setenv("RUNNER_TEST_OVERRIDDEN", "old")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Command{
		Name: "sh",
		Args: []string{"-c", `pwd -P; echo "$RUNNER_TEST_INHERITED $RUNNER_TEST_OVERRIDDEN $RUNNER_TEST_EXTRA"`},
		Dir:  dir,
		Env:  []string{"RUNNER_TEST_OVERRIDDEN=new", "RUNNER_TEST_EXTRA=extra"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(res.Stdout), dir+"\ninherited new extra\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunOutputIsBounded(t *testing.T) {
	t.Parallel()
	c := helper("lines")
	c.MaxOutput = 1000
	res, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		name      string
		data      []byte
		truncated bool
		last      string
	}{
		{"stdout", res.Stdout, res.StdoutTruncated, "out 5000\n"},
		{"stderr", res.Stderr, res.StderrTruncated, "err 5000\n"},
	} {
		if len(s.data) != 1000 {
			t.Errorf("%s: kept %d bytes, want exactly the 1000-byte cap", s.name, len(s.data))
		}
		if !s.truncated {
			t.Errorf("%s: not reported as truncated", s.name)
		}
		if !bytes.HasSuffix(s.data, []byte(s.last)) {
			t.Errorf("%s: the kept part is not the tail; it ends with %q", s.name, s.data[len(s.data)-20:])
		}
	}
}

func TestRunDefaultCapHoldsAgainstEndlessOutput(t *testing.T) {
	t.Parallel()
	c := helper("flood")
	c.Timeout = 500 * time.Millisecond
	res, err := Run(context.Background(), c)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if len(res.Stdout) > DefaultMaxOutput {
		t.Errorf("kept %d bytes, more than the default cap %d", len(res.Stdout), DefaultMaxOutput)
	}
}

func TestCaptureMemoryStaysBounded(t *testing.T) {
	t.Parallel()
	const max = 4096
	c := newCapture(max, Stdout, nil, nil)
	chunk := bytes.Repeat([]byte("x"), 1500)
	for i := 0; i < 10000; i++ {
		c.write(chunk)
		if len(c.buf) > 2*max {
			t.Fatalf("buffer holds %d bytes after write %d, want at most %d", len(c.buf), i, 2*max)
		}
	}
	if cap(c.buf) > 4*max {
		t.Errorf("buffer capacity grew to %d for a cap of %d", cap(c.buf), max)
	}
	out, truncated := c.result()
	if len(out) != max || !truncated {
		t.Errorf("result: %d bytes, truncated %v; want %d, true", len(out), truncated, max)
	}
}

func TestRunOnLine(t *testing.T) {
	t.Parallel()
	// Calls are serialized by Run, so the callback appends without a lock;
	// the race detector checks that claim. Lines are compared per stream
	// because the two streams are read independently.
	got := map[Stream][]string{}
	res, err := Run(context.Background(), Command{
		Name: "sh",
		// The unterminated "last" is delivered when the stream ends.
		Args: []string{"-c", `printf 'one\ntwo\r\nthree\rfour\n\nlast'; printf 'warn\nagain\n' >&2`},
		OnLine: func(s Stream, text string) {
			got[s] = append(got[s], text)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[Stream][]string{
		Stdout: {"one", "two", "three", "four", "", "last"},
		Stderr: {"warn", "again"},
	}
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	// The callback does not replace the capture.
	if string(res.Stdout) != "one\ntwo\r\nthree\rfour\n\nlast" || string(res.Stderr) != "warn\nagain\n" {
		t.Errorf("captured stdout %q, stderr %q", res.Stdout, res.Stderr)
	}
}

func TestCaptureCapsLineLength(t *testing.T) {
	t.Parallel()
	var lines []string
	var mu sync.Mutex
	c := newCapture(1024, Stdout, func(_ Stream, s string) { lines = append(lines, s) }, &mu)
	long := bytes.Repeat([]byte("y"), 3*maxLineBytes)
	c.write(long)
	if len(c.line) != maxLineBytes {
		t.Fatalf("pending line holds %d bytes, want the cap %d", len(c.line), maxLineBytes)
	}
	c.write([]byte("\nshort\n"))
	if len(lines) != 2 || len(lines[0]) != maxLineBytes || lines[1] != "short" {
		t.Errorf("got %d lines, first %d bytes long", len(lines), len(lines[0]))
	}
}

func TestRunDoesNotHangOnEscapedDescendant(t *testing.T) {
	// Not parallel: it shortens pipeGrace, which parallel tests read. They
	// only start after every sequential test has finished.
	old := pipeGrace
	pipeGrace = 300 * time.Millisecond
	defer func() { pipeGrace = old }()

	start := time.Now()
	res, err := Run(context.Background(), helper("escape"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	// The grandchild left the process group, so Run cannot kill it. Do it
	// here; it would exit by itself after 20s anyway.
	pid := parsePid(t, res.Stdout)
	defer syscall.Kill(pid, syscall.SIGKILL)

	if !alive(pid) {
		t.Fatal("the grandchild is gone; the test did not exercise an open pipe")
	}
	if elapsed > 10*time.Second {
		t.Errorf("returned after %v; Run waited for the pipe held by the grandchild", elapsed)
	}
}

func TestErrorMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  Error
		want string
	}{
		{Error{Name: "ffmpeg", ExitCode: 1}, "run ffmpeg: exit code 1"},
		{Error{Name: "ffmpeg", ExitCode: 1, Stderr: "a\nb"}, "run ffmpeg: exit code 1; stderr tail:\na\nb"},
		{Error{Name: "ffmpeg", ExitCode: -1, Err: context.DeadlineExceeded}, "run ffmpeg: timed out and was killed"},
		{Error{Name: "ffmpeg", ExitCode: -1, Err: context.Canceled}, "run ffmpeg: cancelled and was killed"},
		{Error{Name: "ffmpeg", ExitCode: -1, Err: errors.New("no such file")}, "run ffmpeg: no such file"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestStreamString(t *testing.T) {
	t.Parallel()
	if Stdout.String() != "stdout" || Stderr.String() != "stderr" {
		t.Errorf("got %q and %q", Stdout, Stderr)
	}
}
