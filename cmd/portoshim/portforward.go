package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
)

// podNetNSPath returns the path of the network namespace of the pod sandbox
// portoid, or "" if the pod lives in the host network namespace.
//
// Pods created with NetType "netns" carry a named netns ("netns cni-XXX" in
// the porto "net" property) under Cfg.CNI.NetnsDir. Pods with porto-managed
// "L3" networking have an anonymous netns, which is reached through a task
// running in the pod.
func podNetNSPath(ctx context.Context, portoid string) (string, error) {
	pc := getPortoClient(ctx)

	netProp, err := pc.GetProperty(portoid, "net")
	if err != nil {
		return "", fmt.Errorf("get %s net property: %w", portoid, err)
	}

	if name := parsePropertiesNetNS(netProp, "netns"); name != "" {
		return filepath.Join(Cfg.CNI.NetnsDir, name), nil
	}
	if parsePropertiesNetNS(netProp, "L3") == "" {
		return "", nil
	}

	// The pod container itself may be a meta container without a task, so
	// fall back to its children; all of them share the pod netns.
	names := []string{portoid}
	children, err := pc.ListContainers(portoid + "/***")
	if err != nil {
		return "", fmt.Errorf("list %s children: %w", portoid, err)
	}
	names = append(names, children...)

	for _, name := range names {
		rootPid, err := pc.GetProperty(name, "root_pid")
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(rootPid))
		if err != nil || pid <= 0 {
			continue
		}
		return fmt.Sprintf("/proc/%d/ns/net", pid), nil
	}

	return "", fmt.Errorf("no running task found in pod %s to enter its netns", portoid)
}

// dialInNetNS connects to localhost:port from inside the network namespace at
// nsPath ("" means the current one). The socket keeps its namespace after the
// thread switches back, so the connection is usable from any goroutine.
func dialInNetNS(nsPath string, port int32) (net.Conn, error) {
	dial := func() (net.Conn, error) {
		var errs []error
		for _, host := range []string{"127.0.0.1", "::1"} {
			conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}

	if nsPath == "" {
		return dial()
	}

	type result struct {
		conn net.Conn
		err  error
	}
	resCh := make(chan result, 1)

	// setns(CLONE_NEWNET) affects only the calling thread, so the work runs on
	// a dedicated locked OS thread. If switching back fails the thread stays
	// locked and the Go runtime discards it when the goroutine exits.
	go func() {
		runtime.LockOSThread()

		conn, restored, err := dialFromNetNS(nsPath, dial)
		if restored {
			runtime.UnlockOSThread()
		}
		resCh <- result{conn, err}
	}()

	res := <-resCh
	return res.conn, res.err
}

// dialFromNetNS must run on a locked OS thread. restored reports whether the
// thread is back in its original netns.
func dialFromNetNS(nsPath string, dial func() (net.Conn, error)) (conn net.Conn, restored bool, err error) {
	origFd, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, true, fmt.Errorf("open current netns: %w", err)
	}
	defer unix.Close(origFd)

	targetFd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, true, fmt.Errorf("open %s: %w", nsPath, err)
	}
	defer unix.Close(targetFd)

	if err := unix.Setns(targetFd, unix.CLONE_NEWNET); err != nil {
		return nil, true, fmt.Errorf("setns %s: %w", nsPath, err)
	}

	conn, dialErr := dial()

	if err := unix.Setns(origFd, unix.CLONE_NEWNET); err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, false, fmt.Errorf("restore netns: %w", err)
	}

	if dialErr != nil {
		return nil, true, fmt.Errorf("dial port in %s: %w", nsPath, dialErr)
	}
	return conn, true, nil
}

// copyPortForward pumps data between the kubelet stream and the pod
// connection until the pod side closes, the stream fails or ctx is done.
func copyPortForward(ctx context.Context, conn net.Conn, stream io.ReadWriteCloser) error {
	inCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, stream)
		// Client finished sending: half-close so the pod sees EOF but can
		// still send its response.
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
		inCh <- err
	}()

	outCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(stream, conn)
		outCh <- err
	}()

	for {
		select {
		case err := <-outCh:
			return err
		case err := <-inCh:
			if err != nil {
				return fmt.Errorf("copy from client: %w", err)
			}
			inCh = nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
