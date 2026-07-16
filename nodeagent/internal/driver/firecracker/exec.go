//go:build firecracker && linux

package firecracker

import (
	"bytes"
	"os/exec"
	"syscall"
)

// commandRunner is the seam between the driver's state machine and the host
// toolchain it drives (ip/nft, cryptsetup, mount/mkfs, the jailer, pgrep).
// Production uses osRunner; unit tests install a fake (see
// statemachine_unit_test.go) so boot/stop/reattach run without root or KVM —
// the root+KVM integration tests keep covering the real toolchain.
type commandRunner interface {
	// CombinedOutput runs name args... with optional stdin, returning combined
	// stdout+stderr — the error-context flavor used by run and cryptsetup.
	CombinedOutput(stdin []byte, name string, args ...string) ([]byte, error)
	// Output runs name args... and returns stdout only (runOut).
	Output(name string, args ...string) ([]byte, error)
}

// The process-probing/killing and mount-table seams live next to cmds: they are
// kernel state rather than exec'd commands, but the state machine branches on
// them the same way.
var (
	cmds         commandRunner = osRunner{}
	processAlive               = processAliveOS
	killProcess                = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	isMounted                  = isMountedProc
	mapperExists               = fileExists // /dev/mapper node probe
)

type osRunner struct{}

func (osRunner) CombinedOutput(stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

func (osRunner) Output(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// processAliveOS reports whether pid refers to a live process (signal 0 probe).
func processAliveOS(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
