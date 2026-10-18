package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ManagedProcessStopGrace is the shared grace period before escalating
// provider-managed process termination from SIGTERM to SIGKILL.
const ManagedProcessStopGrace = 5 * time.Second

// ManagedProcessReapGrace bounds how long a kill waits, after SIGKILL, for the
// target to actually leave the run/ready set before reporting it as
// not-confirmed-dead. A process wedged in uninterruptible sleep (D-state) under
// I/O can outlive its own SIGKILL until the I/O completes; waiting for
// confirmed death (gone or zombie) before starting a replacement is what keeps
// an escaped old process from racing the new one for the same work bead.
const ManagedProcessReapGrace = 3 * time.Second

// SignalProcessGroup sends sig to the managed process group when possible and
// falls back to the direct process signal for older sessions or platforms that
// cannot signal by group.
func SignalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}

// TerminateManagedProcess sends SIGTERM, waits for done, then escalates to
// SIGKILL after grace if the process group is still alive. The provider supplies
// readIdentity to identify the original process without coupling this contract
// package to a platform-specific process lookup.
func TerminateManagedProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration, readIdentity func(int) (string, error)) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if readIdentity == nil {
		return fmt.Errorf("cannot confirm managed process identity: no identity reader")
	}
	pid := cmd.Process.Pid
	identity, _ := readIdentity(pid)
	pgid, groupErr := syscall.Getpgid(pid)
	// The numeric PID may have been reused after this exec.Cmd was reaped.
	// Require its original os.Process handle to still identify a live leader.
	originalAlive := cmd.Process.Signal(syscall.Signal(0)) == nil
	ownedGroup := groupErr == nil && pgid == pid && identity != "" && originalAlive
	if !ownedGroup && !errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
		// A reaped leader can leave live descendants. Without its identity,
		// neither a group signal nor a successful stop is justified.
		return fmt.Errorf("cannot confirm managed process group %d ownership", pid)
	}
	_ = SignalProcessGroup(cmd, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		if !ownedGroup || errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return nil
		}
		// The shell may exit while a signal-catching child still owns work.
		<-timer.C
		return signalExitedManagedGroup(pid, identity, readIdentity, syscall.Kill)
	case <-timer.C:
	}

	if ownedGroup {
		if err := signalExitedManagedGroup(pid, identity, readIdentity, syscall.Kill); err != nil {
			return err
		}
	} else if err := SignalProcessGroup(cmd, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	<-done
	return nil
}

// Recheck the original identity before escalation, even if done and the grace
// timer become ready together. Never fall back to a reaped direct PID.
func signalExitedManagedGroup(pid int, identity string, readIdentity func(int) (string, error), kill func(int, syscall.Signal) error) error {
	current, err := readIdentity(pid)
	if err == nil && current != identity {
		return nil // A recycled leader PID means the original group is gone.
	}
	if err != nil && !errors.Is(kill(pid, 0), syscall.ESRCH) {
		return fmt.Errorf("cannot confirm stopped process group %d identity: %w", pid, err)
	}
	if err := kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("killing stopped process group %d: %w", pid, err)
	}
	return nil
}
