//go:build windows

package supervisor

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The server is created suspended, assigned to a job that dies with this
// process, then resumed. Kill uses TerminateJobObject so children die too.
// Interrupt is not a confirmed Plains of Pain clean stop and is not sent.

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED,
	}
}

func startProcess(cmd *exec.Cmd) (func() error, func() error, error) {
	prepareCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return failStart(0, cmd, err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return failStart(job, cmd, err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return failStart(job, cmd, err)
	}
	assignErr := windows.AssignProcessToJobObject(job, proc)
	closeErr := windows.CloseHandle(proc)
	if assignErr != nil {
		return failStart(job, cmd, assignErr)
	}
	if closeErr != nil {
		return failStart(job, cmd, closeErr)
	}
	if err := resumeProcess(uint32(cmd.Process.Pid)); err != nil {
		termErr := windows.TerminateJobObject(job, 1)
		closeJobErr := windows.CloseHandle(job)
		if termErr != nil || closeJobErr != nil {
			return nil, nil, fmt.Errorf("%w (terminate: %v, close: %v)", err, termErr, closeJobErr)
		}
		return nil, nil, err
	}
	kill := func() error {
		return windows.TerminateJobObject(job, 1)
	}
	closeJob := func() error {
		return windows.CloseHandle(job)
	}
	return kill, closeJob, nil
}

func failStart(job windows.Handle, cmd *exec.Cmd, cause error) (func() error, func() error, error) {
	var killErr, closeErr error
	if cmd != nil && cmd.Process != nil {
		killErr = cmd.Process.Kill()
	}
	if job != 0 {
		closeErr = windows.CloseHandle(job)
	}
	if killErr != nil || closeErr != nil {
		return nil, nil, fmt.Errorf("%w (kill: %v, close: %v)", cause, killErr, closeErr)
	}
	return nil, nil, cause
}

func resumeProcess(pid uint32) (err error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := windows.CloseHandle(snap); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err = windows.Thread32First(snap, &entry); err != nil {
		return err
	}
	resumed := false
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr == nil {
				_, resumeErr := windows.ResumeThread(thread)
				closeErr := windows.CloseHandle(thread)
				if resumeErr == nil {
					resumed = true
				}
				if closeErr != nil && err == nil {
					err = closeErr
				}
			}
		}
		if nextErr := windows.Thread32Next(snap, &entry); nextErr != nil {
			break
		}
	}
	if !resumed {
		if err != nil {
			return err
		}
		return errors.New("could not resume the suspended server process")
	}
	return err
}

func interruptProcess(*exec.Cmd) error {
	return errors.New("interrupt stop is not available on Windows; a clean Plains of Pain stop is unverified")
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
