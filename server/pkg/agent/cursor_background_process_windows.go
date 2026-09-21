//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type cursorWindowsBackgroundProcess struct{ job windows.Handle }

var cursorIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func cursorProcessInJob(process, job windows.Handle) (bool, error) {
	var member int32
	ok, _, err := cursorIsProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return false, err
	}
	return member != 0, nil
}

func cursorProcessCreated(process windows.Handle) (uint64, error) {
	var created, exited, kernel, user windows.Filetime
	err := windows.GetProcessTimes(process, &created, &exited, &kernel, &user)
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), err
}

// jobObjectBasicProcessIDList mirrors JOBOBJECT_BASIC_PROCESS_ID_LIST. The
// kernel writes as many PIDs as fit after the two counts; ProcessIdList is the
// first slot of that trailing array, and callers size the buffer behind it.
type jobObjectBasicProcessIDList struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
	ProcessIdList             [1]uintptr
}

// cursorJobProcessIDs lists the processes currently assigned to job. Every
// descendant of a launch inherits its Job at creation and cannot leave it, so
// this is the authoritative record of what belongs to the launch — unlike the
// machine-wide parent-PID snapshot, which keeps a child's recorded parent after
// that parent exits and lets a reused PID point at a process owned by someone
// else (MUL-7417).
func cursorJobProcessIDs(job windows.Handle) (map[uint32]struct{}, error) {
	const word = int(unsafe.Sizeof(uintptr(0)))
	const header = int(unsafe.Sizeof(jobObjectBasicProcessIDList{})) - word
	capacity := 256
	for attempt := 0; ; attempt++ {
		// A []uintptr backing array keeps the header pointer-aligned.
		words := make([]uintptr, (header+word-1)/word+capacity)
		size := uint32(len(words) * word)
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&words[0])), size, nil)
		list := (*jobObjectBasicProcessIDList)(unsafe.Pointer(&words[0]))
		if errors.Is(err, windows.ERROR_MORE_DATA) && attempt < 8 {
			capacity = int(list.NumberOfAssignedProcesses) + 64
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("query job process ids: %w", err)
		}
		count := int(list.NumberOfProcessIdsInList)
		if count > capacity {
			count = capacity
		}
		ids := unsafe.Slice(&list.ProcessIdList[0], capacity)[:count]
		members := make(map[uint32]struct{}, count)
		for _, id := range ids {
			members[uint32(id)] = struct{}{}
		}
		return members, nil
	}
}

func captureCursorBackgroundProcess(cmd *exec.Cmd, pid int) (*cursorBackgroundProcess, error) {
	if cmd == nil || cmd.Process == nil || pid <= 0 || uint64(pid) > uint64(^uint32(0)) || pid == cmd.Process.Pid {
		return nil, errCursorBackgroundProcessInvalid
	}
	root, ok := lookupProcessTree(cmd)
	if !ok {
		return nil, errCursorBackgroundProcessInvalid
	}
	type heldProcess struct {
		pid     uint32
		handle  windows.Handle
		created uint64
	}
	var held []heldProcess
	defer func() {
		for _, p := range held {
			_ = windows.CloseHandle(p.handle)
		}
	}()
	// open is only ever asked for a PID the launch Job listed. Membership is
	// re-checked on the handle so a PID reused between the listing and the open
	// is refused rather than claimed.
	open := func(pid uint32) (heldProcess, error) {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
		if err != nil {
			return heldProcess{}, err
		}
		created, err := cursorProcessCreated(h)
		member, memberErr := cursorProcessInJob(h, root.job)
		if err != nil || memberErr != nil || !member {
			_ = windows.CloseHandle(h)
			return heldProcess{}, errCursorBackgroundProcessInvalid
		}
		p := heldProcess{pid, h, created}
		held = append(held, p)
		return p, nil
	}
	// Membership of this launch's Job is the ownership proof: only processes
	// descended from cmd can be in it, and a payload cannot name another
	// task's process. No walk up the parent chain is needed, and none is
	// attempted — that walk is what a reused PID could derail.
	members, err := cursorJobProcessIDs(root.job)
	if err != nil {
		return nil, err
	}
	if _, ok := members[uint32(pid)]; !ok {
		return nil, errCursorBackgroundProcessInvalid
	}
	target, err := open(uint32(pid))
	if err != nil {
		return nil, err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = windows.CloseHandle(job)
		}
	}()
	if err := windows.AssignProcessToJobObject(job, target.handle); err != nil {
		return nil, err
	}
	// Assign each parent BEFORE enumerating its immediate children. New children
	// now inherit the Job, and pre-existing children are attached breadth first.
	// Candidates come from the launch Job, so a stale parent PID in the snapshot
	// can at worst point at another launch member, never at a foreign process;
	// creation order then tells a real child from one that only points at this
	// PID because its own parent exited and the PID was reused.
	queue := []heldProcess{target}
	claimed := map[uint32]bool{target.pid: true}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		members, err := cursorJobProcessIDs(root.job)
		if err != nil {
			return nil, err
		}
		parents, err := cursorWindowsProcessParents()
		if err != nil {
			return nil, err
		}
		for childPID := range members {
			if claimed[childPID] || parents[childPID] != parent.pid {
				continue
			}
			child, err := open(childPID)
			if err != nil {
				// Listed a moment ago, gone or reused now: there is nothing of
				// ours behind this PID to claim. Skipping it keeps the rest of
				// the subtree owned instead of failing the whole capture.
				continue
			}
			if child.created < parent.created {
				continue
			}
			claimed[childPID] = true
			member, err := cursorProcessInJob(child.handle, job)
			if err != nil {
				return nil, err
			}
			if member {
				continue
			}
			if err := windows.AssignProcessToJobObject(job, child.handle); err != nil {
				return nil, err
			}
			queue = append(queue, child)
		}
	}
	// A failed capture must not kill a healthy shell. Enable kill-on-close only
	// after the whole observed subtree has been claimed successfully.
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return nil, err
	}
	success = true
	return &cursorBackgroundProcess{platform: &cursorWindowsBackgroundProcess{job: job}}, nil
}

func (p *cursorWindowsBackgroundProcess) alive() (bool, error) {
	active, err := (ownedProcessTree{job: p.job}).activeProcesses()
	return active > 0, err
}
func (p *cursorWindowsBackgroundProcess) terminate() error {
	if err := windows.TerminateJobObject(p.job, 1); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	for {
		alive, err := p.alive()
		if err != nil || !alive {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Cursor background job did not exit after one second")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (p *cursorWindowsBackgroundProcess) close() { _ = windows.CloseHandle(p.job) }

func cursorWindowsProcessParents() (map[uint32]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	parents := make(map[uint32]uint32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	for {
		parents[entry.ProcessID] = entry.ParentProcessID
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return parents, nil
			}
			return nil, err
		}
	}
}
