package transcribe

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"time"
)

var lookPath = exec.LookPath

// run starts a child in its own process group, passes its stderr to onLine
// line by line (whisper-cli prints progress there) and returns the tail of
// stderr. When ctx is cancelled the whole group is killed and the error is
// the cancellation cause.
func run(ctx context.Context, name string, args []string, onLine func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var tail []string
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if onLine != nil {
			onLine(line)
		}
		if tail = append(tail, line); len(tail) > 20 {
			tail = tail[1:]
		}
	}

	err = cmd.Wait()
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	return strings.Join(tail, "\n"), err
}
