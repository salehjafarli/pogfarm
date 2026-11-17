package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultMaxOutput is the number of bytes kept per stream when
// Command.MaxOutput is zero.
const DefaultMaxOutput = 1 << 20

const (
	// maxLineBytes caps one line handed to Command.OnLine. The rest of a
	// longer line is dropped.
	maxLineBytes = 64 << 10
	// errTailLines and errTailBytes bound the stderr tail kept in an Error.
	errTailLines = 20
	errTailBytes = 4 << 10
)

// pipeGrace is how long Run waits for the output pipes to drain after the
// process is gone. Only a descendant that left the process group and still
// holds a pipe can make Run wait this long. Tests shorten it.
var pipeGrace = 2 * time.Second

// Stream names one of the two output streams of a command.
type Stream int

const (
	Stdout Stream = iota + 1
	Stderr
)

func (s Stream) String() string {
	switch s {
	case Stdout:
		return "stdout"
	case Stderr:
		return "stderr"
	}
	return fmt.Sprintf("Stream(%d)", int(s))
}

// Command describes one run of an external program.
type Command struct {
	// Name is the program: a path, or a name looked up in PATH.
	Name string
	// Args are passed to the program as they are. No shell is involved.
	Args []string
	// Dir is the working directory. Empty means the current one.
	Dir string
	// Env holds extra "KEY=value" entries added to the environment of this
	// process. An entry here wins over an inherited one with the same key.
	Env []string
	// Timeout kills the command when it runs longer. Zero means no timeout
	// beyond the context's own.
	Timeout time.Duration
	// MaxOutput is the number of bytes kept per stream. When a stream
	// produces more, the oldest bytes are dropped and the tail is kept.
	// Zero means DefaultMaxOutput.
	MaxOutput int
	// OnLine, if set, is called for every line of output as it arrives,
	// without the line ending. Lines end at "\n", "\r\n" or a bare "\r"
	// (ffmpeg rewrites its progress line with "\r"). Calls are serialized,
	// so the callback needs no locking of its own, but it runs on the
	// goroutines reading the pipes: a slow callback slows the command down.
	OnLine func(stream Stream, line string)
}

// Result is what a command produced. Run returns it for failed runs too.
type Result struct {
	// Stdout and Stderr hold the captured output, at most MaxOutput bytes
	// each. When a stream was longer they hold its tail.
	Stdout []byte
	Stderr []byte
	// StdoutTruncated and StderrTruncated report that the stream was longer
	// than MaxOutput and its beginning was dropped.
	StdoutTruncated bool
	StderrTruncated bool
	// ExitCode is the exit status. It is -1 when the process did not start
	// or was killed by a signal.
	ExitCode int
	// Duration is the time from start until the process and its output
	// were finished.
	Duration time.Duration
}

// Error is returned by Run when the command did not start, exited with a
// non-zero status, timed out or was cancelled.
type Error struct {
	// Name is the program that was run.
	Name string
	// ExitCode is the exit status, -1 when the process did not start or
	// was killed by a signal.
	ExitCode int
	// Stderr is the last lines of the command's stderr.
	Stderr string
	// Err is the cause when it is not just a non-zero exit:
	// context.DeadlineExceeded for a timeout, context.Canceled for a
	// cancellation, or the error from starting the process. errors.Is sees
	// it through Unwrap.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("run ")
	b.WriteString(e.Name)
	b.WriteString(": ")
	switch {
	case errors.Is(e.Err, context.DeadlineExceeded):
		b.WriteString("timed out and was killed")
	case errors.Is(e.Err, context.Canceled):
		b.WriteString("cancelled and was killed")
	case e.Err != nil:
		b.WriteString(e.Err.Error())
	}
	if e.Err == nil || e.ExitCode >= 0 {
		if e.Err != nil {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "exit code %d", e.ExitCode)
	}
	if e.Stderr != "" {
		b.WriteString("; stderr tail:\n")
		b.WriteString(e.Stderr)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Run starts the command and waits for it. It returns the captured output
// together with a nil error when the command exits with status 0, and with an
// *Error otherwise.
//
// The command runs in its own process group. When ctx is done or the timeout
// passes, the whole group is killed. The group is also killed once the
// command itself has exited, so a helper it left running in the background
// does not outlive the call.
func Run(ctx context.Context, c Command) (Result, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	maxOut := c.MaxOutput
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}
	res := Result{ExitCode: -1}
	fail := func(err error, stderr string) (Result, error) {
		return res, &Error{Name: c.Name, ExitCode: res.ExitCode, Stderr: stderr, Err: err}
	}

	if err := ctx.Err(); err != nil {
		return fail(err, "")
	}

	cmd := exec.Command(c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	setProcessGroup(cmd)

	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(err, "")
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return fail(err, "")
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	start := time.Now()
	err = cmd.Start()
	// The child has its own copies of the write ends. Ours must go, or the
	// readers never see the end of the stream.
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return fail(err, "")
	}

	var lineMu sync.Mutex
	stdout := newCapture(maxOut, Stdout, c.OnLine, &lineMu)
	stderr := newCapture(maxOut, Stderr, c.OnLine, &lineMu)
	readersDone := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); stdout.readFrom(outR) }()
	go func() { defer readers.Done(); stderr.readFrom(errR) }()
	go func() { readers.Wait(); close(readersDone) }()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr, ctxErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		killGroup(cmd)
		waitErr = <-waitDone
		if waitErr != nil {
			// A command that finished cleanly just as the context ended
			// still counts as a success.
			ctxErr = ctx.Err()
		}
	}
	// The command itself is gone. Kill whatever it left behind in its group.
	killGroup(cmd)

	// With the group dead the pipes reach their end at once. Only a
	// descendant outside the group can still hold one open; do not wait for
	// it forever.
	grace := time.NewTimer(pipeGrace)
	select {
	case <-readersDone:
		grace.Stop()
	case <-grace.C:
	}
	// Closing also unblocks a reader still waiting on an open pipe.
	outR.Close()
	errR.Close()
	<-readersDone

	res.Duration = time.Since(start)
	res.Stdout, res.StdoutTruncated = stdout.result()
	res.Stderr, res.StderrTruncated = stderr.result()
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	switch {
	case ctxErr != nil:
		return fail(ctxErr, tail(res.Stderr))
	case waitErr != nil:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && res.ExitCode >= 0 {
			return fail(nil, tail(res.Stderr))
		}
		// Killed by a signal we did not send, or Wait itself failed.
		return fail(waitErr, tail(res.Stderr))
	}
	return res, nil
}

// tail returns the last lines of b for an error message.
func tail(b []byte) string {
	s := strings.TrimRight(string(b), "\r\n")
	if len(s) > errTailBytes {
		s = s[len(s)-errTailBytes:]
		// Drop the line that was cut in the middle.
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	lines := strings.Split(s, "\n")
	if len(lines) > errTailLines {
		lines = lines[len(lines)-errTailLines:]
	}
	return strings.Join(lines, "\n")
}

// capture collects one output stream: a bounded tail of its bytes and, if
// asked for, its lines.
type capture struct {
	max       int
	buf       []byte
	truncated bool

	stream Stream
	onLine func(Stream, string)
	lineMu *sync.Mutex
	line   []byte
	// afterCR is set when the last byte was "\r", so a following "\n"
	// belongs to the same line ending.
	afterCR bool
}

func newCapture(max int, stream Stream, onLine func(Stream, string), mu *sync.Mutex) *capture {
	return &capture{max: max, stream: stream, onLine: onLine, lineMu: mu}
}

// readFrom consumes r until it ends or is closed.
func (c *capture) readFrom(r io.Reader) {
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			c.write(chunk[:n])
		}
		if err != nil {
			break
		}
	}
	if c.onLine != nil && len(c.line) > 0 {
		c.emitLine()
	}
}

func (c *capture) write(p []byte) {
	c.buf = append(c.buf, p...)
	// Let the buffer reach twice the cap before compacting, so the copy is
	// amortized instead of happening on every write.
	if len(c.buf) > 2*c.max {
		n := copy(c.buf, c.buf[len(c.buf)-c.max:])
		c.buf = c.buf[:n]
		c.truncated = true
	}
	if c.onLine == nil {
		return
	}
	for _, b := range p {
		switch {
		case b == '\n' && c.afterCR:
			c.afterCR = false
		case b == '\n' || b == '\r':
			c.afterCR = b == '\r'
			c.emitLine()
		default:
			c.afterCR = false
			if len(c.line) < maxLineBytes {
				c.line = append(c.line, b)
			}
		}
	}
}

func (c *capture) emitLine() {
	line := string(c.line)
	c.line = c.line[:0]
	c.lineMu.Lock()
	defer c.lineMu.Unlock()
	c.onLine(c.stream, line)
}

// result returns the kept tail and whether anything was dropped before it.
func (c *capture) result() ([]byte, bool) {
	if len(c.buf) > c.max {
		c.buf = c.buf[len(c.buf)-c.max:]
		c.truncated = true
	}
	return c.buf, c.truncated
}
