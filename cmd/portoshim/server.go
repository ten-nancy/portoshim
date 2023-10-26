package main

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ten-nancy/porto/src/api/go/porto"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	grpc "google.golang.org/grpc"
)

type PortoshimServer struct {
	socket        string
	listener      net.Listener
	grpcServer    *grpc.Server
	runtimeMapper *PortoshimRuntimeMapper
	imageMapper   *PortoshimImageMapper
	ctx           context.Context
}

func unlinkStaleSocket(socketPath string) error {
	ss, err := os.Stat(socketPath)
	if err == nil {
		if ss.Mode()&os.ModeSocket != 0 {
			err = os.Remove(socketPath)
			if err != nil {
				return fmt.Errorf("failed to remove socket: %v", err)
			}
			zap.S().Info("unlinked staled socket")
			return nil
		} else {
			return fmt.Errorf("some file already exists at path %q and isn't a socket", socketPath)
		}
	} else {
		if os.IsNotExist(err) {
			return nil
		} else {
			return fmt.Errorf("failed to stat socket path: %v", err)
		}
	}
}

func portoClientContext(ctx context.Context) (context.Context, error) {
	portoClient, err := porto.Dial()
	if err != nil {
		return ctx, fmt.Errorf("connect to porto: %v", err)
	}

	portoClient.SetTimeout(Cfg.Porto.SocketTimeout)

	c := ctx
	//nolint:sa1029
	c = context.WithValue(c, "requestId", fmt.Sprintf("%08x", rand.Intn(4294967296)))
	//nolint:sa1029
	c = context.WithValue(c, "portoClient", portoClient)
	return c, nil
}

func getPortoClient(ctx context.Context) porto.PortoAPI {
	return ctx.Value("portoClient").(porto.PortoAPI)
}

func getRequestID(ctx context.Context) string {
	return ctx.Value("requestId").(string)
}

func serverInterceptor(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler) (interface{}, error) {
	start := time.Now()

	ctx, err := portoClientContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	startTime := time.Since(start).Milliseconds()

	InfoLog(ctx, "%s", info.FullMethod)
	if !strings.Contains(info.FullMethod, "PullImage") {
		DebugLog(ctx, "%s", req)
	} else {
		if strings.Contains(info.FullMethod, "v1alpha2") {
			reqPullImage := req.(v1alpha2PullImageRequestType)
			DebugLog(ctx, "%s %s", reqPullImage.GetImage(), reqPullImage.GetAuth().GetUsername())
		} else {
			reqPullImage := req.(v1PullImageRequestType)
			DebugLog(ctx, "%s %s", reqPullImage.GetImage(), reqPullImage.GetAuth().GetUsername())
		}
	}

	h, err := handler(ctx, req)

	DebugLog(ctx, "%+v", h)
	if err != nil {
		WarnLog(ctx, "%v", err)
	}
	InfoLog(ctx, "%s time: %d ms", info.FullMethod, time.Since(start).Milliseconds()-startTime)

	return h, err
}

func NewPortoshimServer(socketPath string) (*PortoshimServer, error) {
	var err error
	zap.S().Info("starting of portoshim initialization")

	server := PortoshimServer{socket: socketPath}
	server.ctx = server.ShutdownCtx()

	server.runtimeMapper, err = NewPortoshimRuntimeMapper()
	if err != nil {
		return nil, err
	}

	if err = os.MkdirAll(Cfg.Portoshim.VolumesDir, 0755); err != nil {
		zap.S().Fatalf("cannot create volumes dir: %v", err)
		return nil, fmt.Errorf("cannot create volumes dir: %v", err)
	}

	if err = unlinkStaleSocket(socketPath); err != nil {
		zap.S().Fatalf("failed to unlink staled socket: %v", err)
	}

	server.listener, err = net.Listen("unix", server.socket)
	if err != nil {
		zap.S().Fatalf("listen error: %s %v", server.socket, err)
		return nil, fmt.Errorf("listen error: %s %v", server.socket, err)
	}

	if err = os.Chmod(server.socket, 0o660); err != nil {
		zap.S().Warnf("chmod error: %s %v", server.socket, err)
		return nil, fmt.Errorf("chmod error: %s %v", server.socket, err)
	}

	server.grpcServer = grpc.NewServer(grpc.UnaryInterceptor(serverInterceptor))
	RegisterServer(&server)

	rand.Seed(time.Now().UnixNano())

	zap.S().Info("portoshim is initialized")
	return &server, nil
}

func (server *PortoshimServer) Serve() error {
	zap.S().Info("starting of portoshim serving")

	go func() {
		if err := server.grpcServer.Serve(server.listener); err != nil {
			zap.S().Fatalf("unable to run GRPC server: %v", err)
		}
	}()

	zap.S().Info("portoshim is serving...")

	<-server.ctx.Done()
	server.Shutdown()

	zap.S().Info("portoshim is shut down")
	return nil
}

func (server *PortoshimServer) Shutdown() {
	zap.S().Info("portoshim is shutting down")

	server.grpcServer.GracefulStop()
}

func (server *PortoshimServer) ShutdownCtx() (ctx context.Context) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, unix.SIGINT, unix.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-sig
		cancel()
	}()
	return
}
