package main

import (
	"reflect"
	"testing"

	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming/portforward"
)

// Without the port-forward protocols the streaming server panics in the
// handshake: "unable to upgrade: serverProtocols is required".
func TestStreamingConfigProtocols(t *testing.T) {
	config := newStreamingConfig("[::1]:0")

	if !reflect.DeepEqual(config.SupportedRemoteCommandProtocols, remotecommandconsts.SupportedStreamingProtocols) {
		t.Errorf("remote command protocols %v", config.SupportedRemoteCommandProtocols)
	}
	if !reflect.DeepEqual(config.SupportedPortForwardProtocols, portforward.SupportedProtocols) {
		t.Errorf("port-forward protocols %v", config.SupportedPortForwardProtocols)
	}
}
