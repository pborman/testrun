// Copyright 2020 Paul Borman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Program testrun is used to repeatedly run a program to help diagnose
// intermitten bugs.  It is designed work with the the Go programming language's
// "go test" testing.
//
// Normally testrun is called with the path to the source of the Go package that
// is under test.  Only a single package is supported.  Testrun changes
// directory to the Go package's directory and builds the test binary from
// there.  The built test binary is placed in a temporary directory created by
// testrun.
//
// The test binary is built as if the following commands were run:
//
//	mkdir $RUNDIR
//	cd $PACKAGEDIR
//	go test -c -o $RUNDER/test.binary
//
// Alternatively, the --binary option can be used to specify the test binary.
// In this case go test is not run.  Testrun makes no requirements of the test
// binary other than it is an executable.
//
// Normally testrun deletes the directories it creates.  If the output of the
// tests is desired, the --dir option can be used to specify where testrun
// should store results.  Normally the specified directory must not exist.  When
// the -f option is provided, testrun will first attempt to remove the specified
// directory prior to checking to see if it already exists.
//
// By default the test is run serially until failure or the test is interrupted.
// The --duration option is used to limit the amount of time the test will run
// and -n is used to specifiy the maximum number of runs.  These may be used
// together.  Further, the --max option can be used to run multiple tests in
// parallel.
//
// If --continue is specified, testrun does not terminate after the first
// failure.  It continues to run until the duration limit (--duration) is
// reached, the maximum number of tests is reached (-n), or an interrupt signal
// is sent.
//
// By default each tests standard output and standard error are written to
// testrun's standard output and standard error.  In addition, when --dir is
// used and the test binary returns with a status code other than 0, the
// standard output and standard error, if any, is written to a subdirectoy of
// the specified directory.
//
// Use the --silent option to prevent test output from being written to
// testrun's standard output and standard error.  The use of --silent does not
// prevent the output being written to files in testrun's directory (--dir).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/openconfig/goyang/pkg/indent"
	"github.com/pborman/getopt/v2"
	"github.com/pborman/options"
)

var flags = &struct {
	N        int           `getopt:"-n=N limit the number of iterations to run"`
	Max      int           `getopt:"--max=P maxium number of iterations to run in parallel"`
	Continue bool          `getopt:"--continue do not exit after first error"`
	Binary   string        `getopt:"--binary=BINARY program to run (do not use go test)"`
	Verbose  bool          `getopt:"-v be a bit more verbose"`
	Silent   bool          `getopt:"-s --silent only report total failures/successes"`
	Duration time.Duration `getopt:"--duration=DURATION stop running after DURATION"`
	Dir      string        `getopt:"--dir=DIR store results in DIR"`
	Force    bool          `getopt:"-f --force force removal of DIR"`
	Timeout  time.Duration `getopt:"--timeout=DURATION maximum time to run a single test"`
	Killout  time.Duration `getopt:"--killout=DURATION time to wait between kill signals"`
	Bench    string        `getopt:"--bench=PATTERN pass --test.bench=PATTERN to the test program"`

	Debug bool `getopt:"--debug used when debugging testrun"`
}{
	Max:     1,
	Timeout: time.Second * 60,
	Killout: time.Second * 2,
}

// killSignals are the signals tried, in order, to kill a test.  A duration of DefaultKillout
var killSignals = []os.Signal{syscall.SIGQUIT, syscall.SIGINT, syscall.SIGKILL}

var (
	timeoutErr = errors.New("test timed out")
	abortedErr = errors.New("aborted")
)

// A Runner is used to run a single instance of the test.
type Runner struct {
	Args     []string      // Arguments to pass to Binary
	Binary   string        // Path to the text program executable
	Dir      string        // Directory for the test's output
	Timeout  time.Duration // How long we wait for each test
	Killout  time.Duration // How long we wait for a kill signal to work
	Preserve bool          // Do not delete the tmp directory

	Cmd    *exec.Cmd // The running command
	Stdout []byte    // Test's standard output
	Stderr []byte    // Test's standard error

	mu    sync.Mutex
	err   error // Error encountered during test
	errCh chan error
}

// Cleanup cleans up after r completes.
func (r *Runner) Cleanup() {
	if !r.Preserve || r.Error() == nil {
		defer os.RemoveAll(r.Dir)
	}
}

// Error returns an error if the test failed.
func (r *Runner) Error() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// setError sets r's  error to err.  If ifNotSet is also passed then the error
// is only set if it has not already been set.
func (r *Runner) setError(err error, m ...setMode) {
	r.mu.Lock()
	if len(m) == 0 || m[0] != ifNotSet || r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

// This trickery is simply to prevent arbitrary const being past to setError.
type setMode struct{ m int }

var (
	ifNotSet = setMode{1}
)

func writefile(path string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	return ioutil.WriteFile(path, data, 0600)
}

// Run runs the command and returns after the process exits or times out and is
// deemed unkillble.
func (r *Runner) Run() {
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		r.setError(err)
		return
	}
	defer r.Cleanup()
	if flags.Verbose {
		fmt.Printf("Run %s %s\n", r.Binary, strings.Join(r.Args, " "))
	}

	var stdout, stderr bytes.Buffer
	r.Cmd.Dir = r.Dir
	r.Cmd.Stdout = &stdout
	r.Cmd.Stderr = &stderr

	defer func() {
		r.Stdout = stdout.Bytes()
		r.Stderr = stderr.Bytes()
		if !r.Preserve || r.Error() == nil {
			return
		}
		if err := writefile(filepath.Join(r.Dir, "stdout"), r.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		if err := writefile(filepath.Join(r.Dir, "stderr"), r.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	r.mu.Lock()
	// r.err will not be nil if we tried to kill ourselves off
	if r.err == nil {
		r.err = r.Cmd.Start()
		if r.err == nil {
			go func() {
				r.errCh <- r.Cmd.Wait()
				close(r.errCh)
			}()
		} else {
			close(r.errCh)
		}
	}
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return
	}
	t := time.NewTimer(r.Timeout)
	defer t.Stop()
	r.Wait(t, killSignals)
}

// Started returns true if the test has been started.
func (r *Runner) Started() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Cmd.Process != nil
}

// Kill sends the signals, one a t time, to r's process.
func (r *Runner) Kill(signals []os.Signal) bool {
	// Pass Wait an expired timer so it will immediately
	// start killing the process
	t := time.NewTimer(1)
	defer t.Stop()
	return r.Wait(t, signals)
}

// Wait waits for the test to complete.  If timer t fires before the
// test completes then it is sent the signals one at a time.
func (r *Runner) Wait(t *time.Timer, signals []os.Signal) bool {
	if !r.Started() {
		return true
	}
	for _, sig := range signals {
		select {
		case err, ok := <-r.errCh:
			if ok {
				r.setError(err, ifNotSet)
			}
			return true
		case <-t.C:
			r.setError(timeoutErr, ifNotSet)
			r.Cmd.Process.Signal(sig)
			t.Reset(r.Killout)
		}
	}
	r.setError(fmt.Errorf("unkillable child: %v", r.Cmd.Process))
	return false
}

// A Builder is used to build the test binary and then run it.
type Builder struct {
	Binary   string
	Dir      string
	Stdout   []byte
	Stderr   []byte
	Preserve bool
	mu       sync.Mutex
	index    int
	running  map[*Runner]struct{}
}

// NewBuilder either returns a new builder or displays an error and exits.
func NewBuilder() *Builder {
	id := uuid.New()
	b := &Builder{
		Dir:     flags.Dir,
		running: map[*Runner]struct{}{},
	}
	if b.Dir == "" {
		b.Dir = filepath.Join(os.TempDir(), "testbuild."+id.String())
	} else {
		b.Preserve = true
		if flags.Force {
			os.RemoveAll(b.Dir)
		}
		if _, err := os.Stat(b.Dir); err == nil {
			fmt.Fprintf(os.Stderr, "%s already exists\n", b.Dir)
			os.Exit(1)
		}
		// Try to make this directory now to handle permission errors.
		if err := os.MkdirAll(b.Dir, 0700); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	return b
}

// BuildTest builds the Go test binary.
func (b *Builder) BuildTest(pkg string) error {
	if err := os.MkdirAll(b.Dir, 0700); err != nil {
		return err
	}
	if b.Binary == "" {
		b.Binary = filepath.Join(b.Dir, "test.binary")
	}
	if err := os.Chdir(pkg); err != nil {
		b.Cleanup()
		return err
	}
	cmd := exec.Command("go", "test", "-c", "-o", b.Binary)
	var bout, berr bytes.Buffer
	cmd.Stdout = &bout
	cmd.Stderr = &berr
	err := cmd.Run()
	b.Stdout = bout.Bytes()
	b.Stderr = berr.Bytes()
	if err != nil {
		b.Cleanup()
	}
	return err
}

// Cleanup cleans up after the tests and then cleans up anything b created.
func (b *Builder) Cleanup() {
	b.mu.Lock()
	for r := range b.running {
		r.Cleanup()
	}
	b.mu.Unlock()
	if !b.Preserve {
		defer os.RemoveAll(b.Dir)
	}
}

// NewRun returns a new Runner that will pass args to the test program.
func (b *Builder) NewRun(args []string) *Runner {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := b.index
	b.index++
	r := &Runner{
		Timeout:  flags.Timeout,
		Killout:  flags.Killout,
		Dir:      filepath.Join(b.Dir, "testrun."+strconv.Itoa(index)),
		Binary:   b.Binary,
		Args:     args,
		Cmd:      exec.Command(b.Binary, args...),
		Preserve: b.Preserve,
		errCh:    make(chan error, 1),
	}
	b.running[r] = struct{}{}
	return r
}

// Run runs the test r.
func (b *Builder) Run(r *Runner) {
	r.Run()
	b.mu.Lock()
	delete(b.running, r)
	b.mu.Unlock()
}

// CallRuners first calls any provided msgs function, if not silent, and
// then calls f with each running r.
func (b *Builder) CallRunners(f func(*Runner), msgs ...func(n int)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.running) == 0 {
		return
	}
	if !flags.Silent {
		for _, mf := range msgs {
			mf(len(b.running))
		}
	}
	for r := range b.running {
		f(r)
	}
}

func usage() {
	getopt.PrintUsage(os.Stderr)
	fmt.Fprintln(os.Stderr, `
Either a Go PACKAGE or a BINARY must be specified.  If --binary is not provided
then "go test" is used to build the test binary in the PACKAGE directory.`)
}

func main() { os.Exit(Main()) }
func Main() int {
	getopt.SetParameters("[PACKAGE] [ARGS...]")
	getopt.SetUsage(usage)
	args := options.RegisterAndParse(flags)
	if flags.Dir != "" {
		dir, err := filepath.Abs(flags.Dir)
		if err != nil {
			fmt.Println(err)
			return 1
		}
		flags.Dir = dir
	}
	if flags.Max <= 0 {
		fmt.Fprintln(os.Stderr, "--max must be postiive")
		return 1
	}
	if flags.N < 0 {
		fmt.Fprintln(os.Stderr, "-n must be postiive")
		return 1
	}

	b := NewBuilder()
	defer b.Cleanup()

	if flags.Binary != "" {
		b.Binary = flags.Binary
	} else if len(args) > 0 {
		err := b.BuildTest(args[0])
		args = args[1:]
		os.Stdout.Write(b.Stdout)
		os.Stderr.Write(b.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	} else {
		usage()
		return 1
	}
	if flags.Bench != "" {
		args = append([]string{"--test.bench=" + flags.Bench}, args...)
	}

	// killTests is used to kill off the remaining tests when we
	// decide to stop testing due to an interrupt or the duration
	// timer firing.
	killing := false
	killTests := func() {
		// Don't report failures from tests we killed while
		// wrapping up.
		killing = true
		b.CallRunners(func(r *Runner) {
			r.Kill(killSignals)
		}, func(n int) {
			fmt.Fprintf(os.Stderr, "kill %d tests\n", n)
		})
	}



	var durationCh <-chan time.Time
	if flags.Duration > 0 {
		durationCh = time.NewTimer(flags.Duration).C
	}

	// Run the tests

	// the wait status of completed tests are sent to resultCh
	resultCh := make(chan *Runner)

	// done is closed if we receive a signal terminating the tests. 
	done := make(chan struct{})

	throttle := make(chan struct{}, flags.Max)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cnt := 0
		for {
			if flags.N > 0 && cnt >= flags.N {
				break
			}
			cnt++
			select {
			case <-done:
				return
			case <-durationCh:
				// Allows signals to kill us now.
				signal.Reset()
				killTests()
				return
			case throttle <- struct{}{}:
			}
			wg.Add(1)
			r := b.NewRun(args)
			go func() {
				b.Run(r)
				resultCh <- r
				<-throttle
				wg.Done()
			}()
		}
	}()

	// Wait for all the tests to complete and then close resultCh
	// so our Loop below knows to exit.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Prefix is how much we indent stdout and stderr when displaying them.
	prefix := []byte("  ")

	var deadCh <-chan time.Time
	var killOnce sync.Once

	sigch := make(chan os.Signal, 2)
	if !flags.Debug {
		signal.Notify(sigch, syscall.SIGINT)
	}

	// Before we exit we need to kill off any stragling tests that are
	// still running.
	defer func() {
		// Try to kill off everything that might still be running.
		b.CallRunners(func(r *Runner) {
			r.Kill([]os.Signal{syscall.SIGKILL})
			r.Cleanup()
		}, func(n int) {
			fmt.Fprintf(os.Stderr, "killing %d stragling tests\n", n)
		})
	}()

	var sig os.Signal
	failed := 0
	passed := 0
Loop:
	for {
		select {
		case <-deadCh:
			fmt.Fprintf(os.Stderr, "timeout waiting for tests to die\n")
			break Loop

		case sig = <-sigch:
			if sig == syscall.SIGCHLD {
				continue
			}
			signal.Reset()
			// Don't exit after the first one, try to collect
			// all the killed tests.
			flags.Continue = true
			killOnce.Do(func() {
				deadCh = time.NewTimer(flags.Killout * time.Duration(len(killSignals)+1)).C
				close(done)
				fmt.Fprintf(os.Stderr, "exiting on signal %v\n", sig)
				killTests()
			})

		case r, ok := <-resultCh:
			if !ok {
				break Loop
			}
			err := r.Error()
			switch {
			case killing:
				// We are killing off processes so they
				// do not count as either failed or passed.
			case err != nil:
				if !flags.Silent {
					fmt.Printf("FAIL: %v\n", err)
					if len(bytes.TrimSpace(r.Stdout)) > 0 {
						fmt.Printf("Stdout:\n%s\n", indent.Bytes(prefix, r.Stdout))
					}
					if len(bytes.TrimSpace(r.Stderr)) > 0 {
						fmt.Printf("Stderr:\n%s\n", indent.Bytes(prefix, r.Stderr))
					}
				}
				failed++
				if !flags.Continue {
					break Loop
				}
			default:
				if flags.Bench != "" {
					if len(bytes.TrimSpace(r.Stdout)) > 0 {
						fmt.Printf("Stdout:\n%s\n", indent.Bytes(prefix, r.Stdout))
					}
					if len(bytes.TrimSpace(r.Stderr)) > 0 {
						fmt.Printf("Stderr:\n%s\n", indent.Bytes(prefix, r.Stderr))
					}
				} 
				passed++
			}
		}
	}
	fmt.Printf("%d/%d PASSED\n", passed, passed+failed)
	if failed > 0 {
		return 1
	}
	return 0
}
