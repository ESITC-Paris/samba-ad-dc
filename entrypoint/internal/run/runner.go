package run

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Runner is the single seam through which the entrypoint shells out (§6.7).
// Everything that leaves the process — samba-tool, samba, chronyd, smbclient
// — goes through it, so the whole state machine is testable without a domain
// controller and the list of external programs stays auditable in one place.
type Runner interface {
	// Run executes name and waits for it to finish.
	Run(ctx context.Context, name string, args ...string) error
	// Start executes name as a daemon and returns before it finishes.
	Start(ctx context.Context, name string, args ...string) (Proc, error)
}

// Proc is a started daemon: it can be signaled and it must be reaped.
type Proc interface {
	Signal(os.Signal) error
	Wait() error
}

// redactedValue replaces a secret in any rendering of a command line.
const redactedValue = "<redacted>"

// secretFlags are the argv flags whose value is a password. samba-tool has no
// way to take these from the environment or from stdin, so they are passed on
// the command line — see the note on provisionArgs in run.go. They are
// redacted in every log line and every error this package produces, because a
// crash report or a CI log outlives the container's process table.
var secretFlags = []string{"--adminpass", "--password"}

// redactArgs returns a copy of args with every password value replaced. The
// input is never modified: the caller still has to run the real command.
func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		for _, flag := range secretFlags {
			if strings.HasPrefix(out[i], flag+"=") {
				out[i] = flag + "=" + redactedValue
				break
			}
			if out[i] == flag && i+1 < len(out) {
				out[i+1] = redactedValue
				i++
				break
			}
		}
	}
	return out
}

// commandLine renders a command for humans with its secrets removed.
func commandLine(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(redactArgs(args), " "))
}

// execRunner is the production Runner: it forks real processes and wires
// their output straight to the container's stdout and stderr (§6.4).
type execRunner struct {
	stdout io.Writer
	stderr io.Writer
}

// NewExecRunner returns the Runner used in the container. stdout and stderr
// receive both the children's output and the one-line record of each command
// started, so an operator reading `docker logs` sees what ran and in which
// order — with passwords redacted.
func NewExecRunner(stdout, stderr io.Writer) Runner {
	return &execRunner{stdout: stdout, stderr: stderr}
}

// Run executes name and waits. The returned error names the command with its
// secrets redacted, never the secret itself.
func (r *execRunner) Run(ctx context.Context, name string, args ...string) error {
	line := commandLine(name, args)
	r.log(line)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", line, err)
	}
	return nil
}

// Start executes name as a daemon. Cancelling ctx asks the child to stop the
// same way Supervise does — with SIGTERM — instead of the SIGKILL os/exec
// would send by default: a killed samba leaves its databases mid-write.
func (r *execRunner) Start(ctx context.Context, name string, args ...string) (Proc, error) {
	line := commandLine(name, args)
	r.log(line)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = r.stdout
	cmd.Stderr = r.stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", line, err)
	}
	return &execProc{cmd: cmd, line: line}, nil
}

// log records one command, already redacted.
func (r *execRunner) log(line string) {
	if r.stdout == nil {
		return
	}
	fmt.Fprintf(r.stdout, "entrypoint: running: %s\n", line)
}

// execProc is a real started process.
type execProc struct {
	cmd  *exec.Cmd
	line string
}

// Signal forwards sig to the child.
func (p *execProc) Signal(sig os.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("%s: process is not running", p.line)
	}
	return p.cmd.Process.Signal(sig)
}

// Wait reaps the child and reports how it ended.
func (p *execProc) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("%s: %w", p.line, err)
	}
	return nil
}
