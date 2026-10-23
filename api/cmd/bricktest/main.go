// Command bricktest checks that shared bricks work inside their built image.
//
// A brick opts in with a tests.py next to its config.yml (see runner.py for
// how tests are written). For each brick, bricktest composes an agent that
// uses only that brick, exactly as the server would, adds the tests and the
// test runner to the image, builds it, and runs the tests in a container.
//
//	go run ./cmd/bricktest [-bricks DIR] [-v] [namespace:name ...]
//
// With no names, every brick that has a tests.py is tested. Secrets the
// brick declares are passed into the container from NAME or
// TREFOIL_SECRET_NAME in the environment when set; tests that need a
// missing one should skip.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	"trefoil/api/internal/bricks"
	"trefoil/api/internal/build"
	"trefoil/api/internal/conf"
	"trefoil/api/internal/schema"
)

//go:embed runner.py
var runner []byte

const testsFile = "tests.py"

func main() {
	dir := flag.String("bricks", "../bricks", "shared bricks folder")
	verbose := flag.Bool("v", false, "show docker build output")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	refs := flag.Args()
	if len(refs) == 0 {
		var err error
		if refs, err = discover(*dir); err != nil {
			fatal(err)
		}
	}
	if len(refs) == 0 {
		fatal(fmt.Errorf("no bricks with a %s in %s", testsFile, *dir))
	}

	validator, err := schema.NewConf()
	if err != nil {
		fatal(err)
	}
	lib := bricks.NewLibrary(*dir)

	var failed []string
	for _, ref := range refs {
		fmt.Printf("=== %s\n", ref)
		if err := testBrick(ctx, lib, validator, *dir, ref, *verbose); err != nil {
			fmt.Printf("--- FAIL %s: %v\n\n", ref, err)
			failed = append(failed, ref)
			continue
		}
		fmt.Printf("--- ok %s\n\n", ref)
	}
	if len(failed) > 0 {
		fmt.Printf("FAIL: %s\n", strings.Join(failed, ", "))
		os.Exit(1)
	}
	fmt.Printf("ok: %d brick(s)\n", len(refs))
}

// discover returns every <namespace>:<name> under dir with a tests.py.
func discover(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*", "*", testsFile))
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, m := range matches {
		brick := filepath.Dir(m)
		refs = append(refs, filepath.Base(filepath.Dir(brick))+":"+filepath.Base(brick))
	}
	slices.Sort(refs)
	return refs, nil
}

func testBrick(ctx context.Context, lib *bricks.Library, v *schema.Validator, dir, ref string, verbose bool) error {
	raw, b, err := lib.Load(ref)
	if err != nil {
		return err
	}
	if err := v.ValidateBrick(raw); err != nil {
		return fmt.Errorf("invalid brick: %w", err)
	}
	ns, name, _ := strings.Cut(ref, ":")
	tests, err := os.ReadFile(filepath.Join(dir, ns, name, testsFile))
	if err != nil {
		return err
	}

	bc, err := testContext(ref, b, tests)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "trefoil-bricktest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	tag := "trefoil-bricktest-" + build.SafeName(ns+"-"+name) + ":latest"
	var buf strings.Builder
	var logs io.Writer = &buf
	if verbose {
		logs = os.Stdout
	}
	fmt.Printf("building %s\n", tag)
	if err := (build.CLI{}).Build(ctx, bc, tmp, tag, logs); err != nil {
		// Without -v the build log is only shown when it's needed.
		fmt.Print(tail(buf.String(), 40))
		return err
	}

	args := []string{"run", "--rm", "--pull=never", "--label", "trefoil.bricktest=1"}
	env := os.Environ()
	for secret := range b.Secrets {
		key := strings.ToUpper(secret)
		val, ok := os.LookupEnv(key)
		if !ok {
			val, ok = os.LookupEnv("TREFOIL_SECRET_" + key)
		}
		if ok {
			// "-e NAME" takes the value from docker's environment, keeping it
			// off the command line.
			args = append(args, "-e", key)
			env = append(env, key+"="+val)
		}
	}
	args = append(args, tag, "python", "/trefoil/harness/brick_test.py", "/trefoil/tests/"+testsFile)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tests: %w", err)
	}
	return nil
}

// testContext composes the image an agent using only this brick would get,
// plus the brick's tests and the test runner.
func testContext(ref string, b *conf.Brick, tests []byte) (*build.Context, error) {
	c := &conf.Conf{Bricks: map[string]conf.Brick{ref: *b}}
	bc, err := build.Compose(c, &conf.Agent{Name: "bricktest-" + ref, Bricks: []string{ref}})
	if err != nil {
		return nil, err
	}
	bc.Files["harness/brick_test.py"] = runner
	bc.Files["tests/"+testsFile] = tests
	bc.Dockerfile += "\n# brick tests\nCOPY harness/brick_test.py /trefoil/harness/brick_test.py\nCOPY tests/ /trefoil/tests/\n"
	return bc, nil
}

func tail(s string, lines int) string {
	parts := strings.SplitAfter(s, "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bricktest:", err)
	os.Exit(1)
}
