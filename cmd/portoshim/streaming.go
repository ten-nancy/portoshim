package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	term "github.com/creack/pty"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming"
)

func NewStreamingServer(addr string) (streaming.Server, error) {
	config := streaming.Config{
		Addr:                            addr,
		StreamIdleTimeout:               15 * time.Minute,
		StreamCreationTimeout:           remotecommandconsts.DefaultStreamCreationTimeout,
		SupportedRemoteCommandProtocols: remotecommandconsts.SupportedStreamingProtocols,
	}
	runtime, _ := NewStreamingRuntime()
	return streaming.NewServer(config, runtime)
}

func NewStreamingRuntime() (StreamingRuntime, error) {
	return StreamingRuntime{}, nil
}

type StreamingRuntime struct{}

func (sr StreamingRuntime) Exec(_ context.Context, containerID string, cmd []string, stdin io.Reader, stdout, stderr io.WriteCloser, terminal bool, resize <-chan remotecommand.TerminalSize) error {
	ctx, err := portoClientContext(context.Background())
	if err != nil {
		return fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	pc := getPortoClient(ctx)
	id := filepath.Join(containerID, createID("exec"))

	// spec initialization for CreateFromSpec
	containerSpec := &pb.TContainerSpec{
		Name: &id,
		Weak: getBoolPointer(true),
	}

	// environment variables
	env, err := pc.GetProperty(containerID, "env")
	if err != nil {
		return fmt.Errorf("failed to get parent container %s env prop: %w", containerID, err)
	}
	prepareExecEnv(ctx, containerSpec, env)

	// command
	if err := prepareCommand(ctx, containerSpec, cmd, nil, nil, true); err != nil {
		return fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	containerSpec.Isolate = getBoolPointer(false)

	var (
		stdinR, stdinW   *os.File
		stdoutR, stdoutW *os.File
		stderrR, stderrW *os.File
		tty, pty         *os.File
	)

	if terminal {
		tty, pty, err = term.Open()
		if err != nil {
			return err
		}
		defer tty.Close()
		defer pty.Close()
	}

	if stdin != nil {
		if terminal {
			stdinR = pty
			stdinW = tty
		} else {
			stdinR, stdinW, err = os.Pipe()
			if err != nil {
				return err
			}
			defer stdinR.Close()
			defer stdinW.Close()
		}

		containerSpec.StdinPath = getStringPointer(fmt.Sprintf("/dev/fd/%d", stdinR.Fd()))
	}

	if stdout != nil {
		if terminal {
			stdoutR = tty
			stdoutW = pty
		} else {
			stdoutR, stdoutW, err = os.Pipe()
			if err != nil {
				return err
			}
			defer stdoutR.Close()
			defer stdoutW.Close()
		}

		containerSpec.StdoutPath = getStringPointer(fmt.Sprintf("/dev/fd/%d", stdoutW.Fd()))
	}

	if terminal {
		stderrR = tty
		stderrW = pty
	} else if stderr != nil {
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			return err
		}
		defer stderrR.Close()
		defer stderrW.Close()
	}

	if stderrW != nil {
		containerSpec.StderrPath = getStringPointer(fmt.Sprintf("/dev/fd/%d", stderrW.Fd()))
	}

	// create and start container
	if err = pc.CreateFromSpec(containerSpec, []*pb.TVolumeSpec{}, true); err != nil {
		return fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}
	defer pc.Destroy(id)

	if !terminal {
		if stdin != nil {
			_ = stdinR.Close()
		}
		if stdout != nil {
			_ = stdoutW.Close()
		}
		if stderr != nil {
			_ = stderrW.Close()
		}
	}

	ctx, cancel := context.WithCancel(ctx)

	copyStream := func(ctx context.Context, dst io.Writer, src io.Reader, name string) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				n, err := io.Copy(dst, src)
				if err != nil {
					zap.S().Warnf("cannot copy %s: %v", name, err)
					return
				}
				if n == 0 {
					return
				}
			}

		}
	}

	if stdin != nil {
		go copyStream(ctx, stdinW, stdin, "stdin")
	}
	if stdout != nil {
		go copyStream(ctx, stdout, stdoutR, "stdout")
	}
	if stderr != nil {
		go copyStream(ctx, stderr, stderrR, "stderr")
	}

	defer cancel()
	_, err = pc.Wait([]string{id}, -1)
	if err != nil {
		zap.S().Warnf("failed to wait %s: %v", id, err)
	}

	return nil
}
func (sr StreamingRuntime) Attach(ctx context.Context, containerID string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	return nil
}
func (sr StreamingRuntime) PortForward(ctx context.Context, podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	return nil
}
