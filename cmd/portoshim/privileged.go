package main

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ten-nancy/porto/src/api/go/porto"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
	"golang.org/x/sys/unix"
)

const (
	sandboxInPriv = "/sandbox_root"
)

var modCaps = pb.TCapabilities{
	Cap:    []string{"SYS_MODULE", "SYS_ADMIN"},
	Action: getBoolPointer(true),
}

var rmModCaps = pb.TCapabilities{
	Cap:    []string{"SYS_MODULE", "SYS_ADMIN"},
	Action: getBoolPointer(false),
}

func getSandboxCommand(ctx context.Context, podID string, pc porto.PortoAPI) string {
	sandboxCmd, err := pc.GetProperty(podID, "command")
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
	}

	sandboxCmd = strings.TrimSpace(sandboxCmd)
	sandboxCmd = strings.TrimPrefix(sandboxCmd, "'")
	sandboxCmd = strings.TrimSuffix(sandboxCmd, "'")
	sandboxCmd = filepath.Join(sandboxInPriv, sandboxCmd)
	return sandboxCmd
}

// remountProcSys enters the target process's mount and PID namespaces via
// setns(2) and remounts /proc/sys read-write using mount(2) directly. This
// avoids relying on the `nsenter`/`mount` binaries, which are not present in
// every container image. Must run on the host with CAP_SYS_ADMIN.
func remountProcSys(ctx context.Context, pid int) error {
	// setns into a mount namespace mutates the calling thread irreversibly,
	// so the work runs on a dedicated, locked OS thread. The thread is never
	// unlocked; the Go runtime tears it down when the goroutine exits.
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		errCh <- enterNsAndRemountProcSys(pid)
	}()
	if err := <-errCh; err != nil {
		return err
	}

	DebugLog(ctx, "Successfully remounted /proc/sys to RW for PID %d\n", pid)
	return nil
}

func enterNsAndRemountProcSys(pid int) error {
	// Open the target's namespace handles. Order matters: PID namespace must
	// be entered before the mount namespace, because once we are inside the
	// target mount namespace we may no longer be able to open files under the
	// host's /proc.
	pidNsPath := fmt.Sprintf("/proc/%d/ns/pid", pid)
	mntNsPath := fmt.Sprintf("/proc/%d/ns/mnt", pid)

	pidFd, err := unix.Open(pidNsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", pidNsPath, err)
	}
	defer unix.Close(pidFd)

	mntFd, err := unix.Open(mntNsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", mntNsPath, err)
	}
	defer unix.Close(mntFd)

	// Go's runtime creates OS threads with CLONE_FS, so every thread shares
	// fs_struct (root, cwd, umask). The kernel refuses setns(CLONE_NEWNS) with
	// EINVAL while that sharing is in place; unshare(CLONE_FS) detaches this
	// thread's fs_struct so the mount-namespace switch is allowed.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare CLONE_FS (pid=%d): %w", pid, err)
	}

	// setns(CLONE_NEWPID) only affects subsequently fork(2)'d children of this
	// thread; it does not change the caller's own PID namespace. We still
	// enter it to match the original nsenter -p behaviour and so any helper
	// we ever spawn from this thread lands in the right namespace.
	if err := unix.Setns(pidFd, unix.CLONE_NEWPID); err != nil {
		return fmt.Errorf("setns pid (pid=%d): %w", pid, err)
	}
	if err := unix.Setns(mntFd, unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("setns mnt (pid=%d): %w", pid, err)
	}

	// /proc/sys is typically masked by a read-only bind mount inside the
	// container, so the remount must carry MS_BIND together with MS_REMOUNT.
	// Omitting MS_RDONLY in the flags clears the read-only bit.
	if err := unix.Mount("", "/proc/sys", "", unix.MS_REMOUNT|unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("remount /proc/sys rw (pid=%d): %w", pid, err)
	}
	return nil
}

func createPrivCnt(ctx context.Context, id string, cmds []string, pc porto.PortoAPI) error {
	id = filepath.Join(id, privCntName)

	privCntSpec := &pb.TContainerSpec{
		Name:         &id,
		Capabilities: &modCaps,
	}

	state, err := pc.GetProperty(id, "state")
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
	}

	if state == portoMetaState || state == portoRunningState || state == portoStoppedState {
		InfoLog(ctx, "container %s exists and in %s state", id, state)
		return nil
	}

	privCntSpec.Isolate = getBoolPointer(false)
	DebugLog(ctx, "prepare privileged command: cmd=%+v", cmds)
	privCntSpec.CommandArgv = &pb.TContainerCommandArgv{
		Argv: cmds,
	}

	volumes := &[]*pb.TVolumeSpec{}
	if err := pc.CreateFromSpec(privCntSpec, *volumes, false); err != nil {
		DebugLog(ctx, "Failed to CreateFromSpec privileged container %v", err)
		return fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	if err := pc.UpdateFromSpec(privCntSpec, true); err != nil {
		DebugLog(ctx, "Failed to UpdateFromSpec privileged container %v", err)
		return fmt.Errorf("%s: failed to start privileged container %v", getCurrentFuncName(), err)
	}

	return nil
}
