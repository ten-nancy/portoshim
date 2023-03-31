package main

const (
	// sockets
	PortoshimSocket = "/run/portoshim.sock"
	PortoSocket     = "/run/portod.socket"
	// common dirs
	LogsDir    = "/var/log/portoshim"
	ImagesDir  = "/place/porto_docker"
	VolumesDir = "/place/portoshim_volumes"
	// cni dirs
	NetworkPluginConfDir = "/etc/cni/net.d"
	NetworkPluginBinDir  = "/opt/cni/bin"
	NetnsDir             = "/var/run/netns"
	// paths
	PortoshimLogPath = LogsDir + "/portoshim.log"
	// k8s
	KubeResourceDomain = "portoshim.net"
	// streaming
	StreamingServerAddress = "[::]"
	StreamingServerPort    = "7255"
)
