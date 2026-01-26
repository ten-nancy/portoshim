package main

import (
	"context"

	cni "github.com/containerd/go-cni"
)

func traceNetworks(ctx context.Context, networks []*cni.ConfNetwork) {
	for _, network := range networks {
		if network == nil {
			continue
		}
		DebugLog(ctx, "netPlugin.Load network %v", network.IFName)
		if network.Config != nil {
			DebugLog(ctx, "Config: %v", *network.Config)
		}
	}
}
