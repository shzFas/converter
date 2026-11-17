package source

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tailBuffer keeps the last bytes written to it: enough of stderr to explain
// a failure without holding megabytes of ffmpeg chatter.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// run starts a child process in its own process group and feeds its stdout
// to onLine line by line. When ctx is cancelled the whole group is killed,
// i.e. yt-dlp together with the ffmpeg it has started.
//
// The returned string is the tail of stderr. If ctx was cancelled the error
// is the cancellation cause.
func run(ctx context.Context, name string, args []string, onLine func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	stderr := &tailBuffer{max: 8 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	reader := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, readErr := reader.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" && onLine != nil {
			onLine(line)
		}
		if readErr != nil {
			break
		}
	}
	// Drain whatever is left so the child never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, reader)

	err = cmd.Wait()
	if ctx.Err() != nil {
		return stderr.String(), context.Cause(ctx)
	}
	return stderr.String(), err
}

// output runs a child process and returns its whole stdout.
func output(ctx context.Context, name string, args []string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second

	var stdout bytes.Buffer
	stderr := &tailBuffer{max: 8 << 10}
	cmd.Stdout, cmd.Stderr = &stdout, stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, stderr.String(), context.Cause(ctx)
	}
	return stdout.Bytes(), stderr.String(), err
}
