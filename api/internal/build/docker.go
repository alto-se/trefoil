package build

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Docker builds and runs images. The CLI implementation shells out to the
// docker binary; tests swap in a fake.
type Docker interface {
	// Build writes the context to dir, builds it, and streams docker's
	// output to logs. dir is kept afterwards for inspection.
	Build(ctx context.Context, bc *Context, dir, tag string, logs io.Writer) error
	Run(ctx context.Context, tag string, stdin []byte, opts RunOptions) (*RunResult, error)
	ImageExists(ctx context.Context, tag string) (bool, error)
}

// ErrImageNotFound is returned by Run when the agent's image hasn't been built.
var ErrImageNotFound = errors.New("image not found")

type RunOptions struct {
	Env     map[string]string
	Timeout time.Duration // zero means no timeout
	// Logs, when set, receives the container's stderr as it is written.
	Logs io.Writer
	// Secrets are set in the container like Env, but passed through the
	// docker process's environment so values stay off its command line.
	Secrets map[string]string
	// Artifacts, when set, is a host directory that the container's
	// ArtifactsDir is copied into once the run ends, even a failed one.
	Artifacts string
}

// ArtifactsDir is where tools in the container put files for the server to
// collect after the run (see the core:trefoil brick).
const ArtifactsDir = "/trefoil/artifacts"

// ExitTimedOut is the exit code reported when a run is killed for
// exceeding its timeout (the same code coreutils `timeout` uses).
const ExitTimedOut = 124

type RunResult struct {
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type CLI struct {
	// Env is set in every container; RunOptions.Env takes precedence.
	Env map[string]string
}

func (CLI) Build(ctx context.Context, bc *Context, dir, tag string, logs io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(bc.Dockerfile), 0o644); err != nil {
		return err
	}
	for name, data := range bc.Files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return err
		}
	}

	cmd := exec.CommandContext(ctx, "docker", "build", "--progress=plain", "-t", tag, dir)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

func (CLI) ImageExists(ctx context.Context, tag string) (bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if strings.Contains(string(out), "No such image") {
		return false, nil
	}
	return false, fmt.Errorf("docker image inspect: %s", strings.TrimSpace(string(out)))
}

func (c CLI) Run(ctx context.Context, tag string, stdin []byte, opts RunOptions) (*RunResult, error) {
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	// A named container can be killed on timeout; killing only the docker
	// CLI process would leave the container running. It is removed here
	// rather than with --rm so artifacts can be copied out after it exits.
	name := "trefoil-run-" + randomSuffix()
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()
	// "-e NAME" forwards the variable from the server's environment when set.
	args := []string{"run", "-i", "--pull=never", "--name", name, "--label", runLabel,
		"-e", "ANTHROPIC_API_KEY", "-e", "ANTHROPIC_BASE_URL",
		// Bedrock credentials: static keys, session token or a Bedrock API key.
		"-e", "AWS_ACCESS_KEY_ID", "-e", "AWS_SECRET_ACCESS_KEY", "-e", "AWS_SESSION_TOKEN",
		"-e", "AWS_REGION", "-e", "AWS_DEFAULT_REGION", "-e", "AWS_BEARER_TOKEN_BEDROCK"}
	// Later -e flags win, so per-run env goes after the settings env.
	for _, env := range []map[string]string{c.Env, opts.Env} {
		for k, v := range env {
			args = append(args, "-e", k+"="+v)
		}
	}
	for k := range opts.Secrets {
		args = append(args, "-e", k)
	}
	cmd := exec.Command("docker", append(args, tag)...)
	if len(opts.Secrets) > 0 {
		cmd.Env = os.Environ()
		for k, v := range opts.Secrets {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if opts.Logs != nil {
		cmd.Stderr = io.MultiWriter(&stderr, opts.Logs)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker run: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		_ = exec.Command("docker", "kill", name).Run()
		err = <-done
	}
	if opts.Artifacts != "" {
		if cpErr := copyArtifacts(name, opts.Artifacts); cpErr != nil && opts.Logs != nil {
			fmt.Fprintf(opts.Logs, "\n[trefoil] collecting artifacts: %v\n", cpErr)
		}
	}

	res := &RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if timedOut {
		res.ExitCode, res.TimedOut = ExitTimedOut, true
		note := fmt.Sprintf("\n[trefoil] run killed after timeout of %s\n", opts.Timeout)
		res.Stderr += note
		if opts.Logs != nil {
			_, _ = io.WriteString(opts.Logs, note)
		}
		return res, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Exit code 125 means docker itself failed, not the container.
		if exitErr.ExitCode() == 125 {
			if strings.Contains(res.Stderr, "No such image") {
				return nil, ErrImageNotFound
			}
			return nil, fmt.Errorf("docker run: %s", strings.TrimSpace(res.Stderr))
		}
		// A non-zero exit from the container is a result, not a failure.
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("docker run: %w", err)
	}
	return res, nil
}

// runLabel marks run containers, so ones left behind when the server stopped
// mid-run can be found by RemoveStoppedRuns.
const runLabel = "trefoil.run=1"

// RemoveStoppedRuns removes run containers that exited without being removed,
// which happens when the server stops while a run is going.
func RemoveStoppedRuns(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "container", "prune", "-f", "--filter", "label="+runLabel).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker container prune: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// copyArtifacts copies ArtifactsDir out of the stopped container into dir.
// A container that never created ArtifactsDir has none, which is no error.
func copyArtifacts(container, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("docker", "cp", container+":"+ArtifactsDir+"/.", dir).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Could not find the file") || strings.Contains(msg, "No such container") {
			return nil
		}
		return fmt.Errorf("docker cp: %s", msg)
	}
	return nil
}
