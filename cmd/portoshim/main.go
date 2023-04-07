package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"go.uber.org/zap"
)

func getCurrentFuncName() string {
	pc, _, _, _ := runtime.Caller(1)
	return runtime.FuncForPC(pc).Name()
}

func main() {
	var err error
	configPath := flag.String("config", "/etc/portoshim.yaml", "YAML config file path")
	debug := flag.Bool("debug", false, "show debug logs")
	flag.Parse()

	if err = InitConfig(*configPath); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "сannot init portoshim config: %v\n", err)
		return
	}

	if err = InitLogger(*debug); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "cannot init logger: %v\n", err)
		return
	}

	if err = InitKnownRegistries(); err != nil {
		zap.S().Fatalf("cannot init known registries: %v", err)
		return
	}

	server, err := NewPortoshimServer(Cfg.Portoshim.Socket)
	if err != nil {
		zap.S().Fatalf("server init error: %v", err)
		return
	}

	if err := server.Serve(); err != nil {
		zap.S().Fatalf("serve error: %v", err)
		return
	}
}
