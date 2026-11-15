package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

func isMissing(err error) bool { return errors.Is(err, os.ErrNotExist) }

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 65536 {
		return 0, errors.New("client help exceeds setup output bound")
	}
	return b.Buffer.Write(p)
}

func ProbeNativePi(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "mcp", "--help")
	cmd.Dir = os.TempDir()
	var output boundedOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return errors.New("Pi MCP capability check failed")
	}
	text := output.String()
	if !strings.Contains(text, "pi mcp") || !strings.Contains(text, "add") {
		return errors.New("this Pi version needs the ohya compatibility extension, which is not installed yet")
	}
	return nil
}
