package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/pkg/netns"
	cni "github.com/containerd/go-cni"
	"github.com/ten-nancy/porto/src/api/go/porto"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestRemovePodSandboxNetwork(t *testing.T) {
	for _, scenario := range []string{"existing_netns", "missing_netns", "retry_after_restart", "missing_sandbox", "missing_config", "ignored_not_found", "host_network"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "existing_netns" && os.Getenv("PORTOSHIM_TEST_NETNS") != "1" {
				t.Skip("requires an isolated Linux mount/net namespace with CAP_SYS_ADMIN")
			}
			root := t.TempDir()
			oldCfg, oldParent := Cfg, parentCnt
			Cfg = &PortoshimConfig{}
			Cfg.CNI.BinDir = root
			Cfg.CNI.ConfDir = filepath.Join(root, "config")
			Cfg.CNI.NetnsDir = filepath.Join(root, "netns")
			Cfg.Portoshim.VolumesDir = filepath.Join(root, "volumes")
			parentCnt = ""
			once = sync.Once{}
			once.Do(func() {})
			t.Cleanup(func() { Cfg, parentCnt = oldCfg, oldParent; once = sync.Once{} })
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			id := "pod-abcd"
			nsPath := filepath.Join(Cfg.CNI.NetnsDir, "gone")
			if scenario == "existing_netns" {
				ns, err := netns.NewNetNS(Cfg.CNI.NetnsDir)
				if err != nil {
					t.Fatal(err)
				}
				nsPath = ns.GetPath()
				t.Cleanup(func() {
					if err := ns.Remove(); err != nil {
						t.Error(err)
					}
				})
			}
			result, fail := filepath.Join(root, "calls"), filepath.Join(root, "fail")
			stopped, cleaned := filepath.Join(root, "stopped"), filepath.Join(root, "cleaned")
			// Exercise the real go-cni Remove with a local plugin and a mocked Porto API.
			plugin := fmt.Sprintf(`#!/bin/sh
set -eu
test "$CNI_COMMAND" = DEL
test "$CNI_CONTAINERID" = pod-abcd
test "$CNI_IFNAME" = veth0
test -z "$CNI_NETNS"
test -f %q
case "$CNI_ARGS" in *K8S_POD_NAMESPACE=fixture*) ;; *) exit 11;; esac
case "$CNI_ARGS" in *K8S_POD_NAME=pod*) ;; *) exit 12;; esac
case "$CNI_ARGS" in *K8S_POD_UID=uid*) ;; *) exit 13;; esac
cat >> %q
if test -f %q; then
 cat %q
 exit 1
fi
touch %q
`, stopped, result, fail, fail, cleaned)
			if scenario == "existing_netns" {
				plugin = strings.Replace(plugin, `test -z "$CNI_NETNS"`, `test -e "$CNI_NETNS"`, 1)
			}
			write(filepath.Join(root, "fixture"), []byte(plugin))
			write(filepath.Join(root, "loopback"), []byte("#!/bin/sh\nexit 0\n"))
			if scenario != "missing_config" {
				write(filepath.Join(Cfg.CNI.ConfDir, "10-primary.conflist"), []byte(`{"cniVersion":"0.3.1","name":"primary","plugins":[{"type":"fixture","capabilities":{"io.kubernetes.cri.pod-annotations":true}}]}`))
			}
			if scenario == "retry_after_restart" {
				write(fail, []byte(`{"cniVersion":"0.3.1","code":42,"msg":"temporary DEL failure"}`))
			}
			if scenario == "ignored_not_found" {
				write(fail, []byte(`{"cniVersion":"0.3.1","code":42,"msg":"policy not found: retry required"}`))
			}
			active := scenario != "missing_sandbox"
			pc := porto.NewMockPortoAPI(gomock.NewController(t))
			pc.EXPECT().GetProperty(id, "net").DoAndReturn(func(_, _ string) (string, error) {
				if !active {
					return "", &porto.PortoError{Code: pb.EError_ContainerDoesNotExist}
				}
				if scenario == "host_network" {
					return "inherited", nil
				}
				return "netns " + filepath.Base(nsPath), nil
			}).AnyTimes()
			var labels []string
			for key, value := range convertToPortoLabels(map[string]string{"io.kubernetes.pod.namespace": "fixture", "io.kubernetes.pod.name": "pod", "io.kubernetes.pod.uid": "uid"}, map[string]string{"network": "fixture"}) {
				labels = append(labels, key+":"+value)
			}
			removeCalls := 1
			if scenario == "missing_sandbox" {
				removeCalls = 0
			} else if scenario == "retry_after_restart" {
				removeCalls = 2
			}
			if scenario != "host_network" {
				pc.EXPECT().GetProperty(id, "labels").Return(strings.Join(labels, ";"), nil).Times(removeCalls)
			}
			pc.EXPECT().StopTimeout(id, time.Duration(0)).DoAndReturn(func(string, time.Duration) error {
				write(stopped, nil)
				return nil
			}).Times(removeCalls)
			if scenario != "missing_sandbox" && scenario != "missing_config" {
				pc.EXPECT().Destroy(id).DoAndReturn(func(string) error {
					if scenario != "host_network" && scenario != "ignored_not_found" {
						if _, err := os.Stat(cleaned); err != nil {
							t.Fatal("Destroy before successful CNI DEL", err)
						}
					}
					if scenario == "existing_netns" {
						if _, err := os.Stat(nsPath); !os.IsNotExist(err) {
							t.Fatal("Destroy before netns cleanup")
						}
					}
					active = false
					return nil
				})
			}
			//nolint:sa1029
			ctx := context.WithValue(context.Background(), "portoClient", pc)
			//nolint:sa1029
			ctx = context.WithValue(ctx, "requestId", t.Name())
			remove := func() error {
				// Each call starts without in-memory CNI networks, as after a restart.
				network, err := cni.New(cni.WithPluginConfDir(Cfg.CNI.ConfDir), cni.WithPluginDir([]string{root}), cni.WithInterfacePrefix(ifPrefixName))
				if err != nil {
					t.Fatal(err)
				}
				m := &PortoshimRuntimeMapper{netPlugin: network}
				_, err = m.RemovePodSandbox(ctx, &v1.RemovePodSandboxRequest{PodSandboxId: id})
				return err
			}
			if scenario == "retry_after_restart" {
				if err := remove(); err == nil || !strings.Contains(err.Error(), "temporary DEL failure") || !active {
					t.Fatalf("failed DEL must preserve sandbox and report error: %v", err)
				}
				if err := os.Remove(fail); err != nil {
					t.Fatal(err)
				}
			}
			err := remove()
			if scenario == "missing_config" {
				if err == nil || !active {
					t.Fatal("missing configuration must block Destroy")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			before, readErr := os.ReadFile(result)
			if scenario == "host_network" || scenario == "missing_sandbox" {
				if !os.IsNotExist(readErr) {
					t.Fatal("unexpected CNI DEL")
				}
			} else if readErr != nil || !strings.Contains(string(before), `"network":"fixture"`) {
				t.Fatal("DEL did not receive pod annotations", string(before), readErr)
			}
			if scenario == "ignored_not_found" {
				// Accepted go-cni behavior: a matching plugin error is treated as success.
				if _, err := os.Stat(cleaned); !os.IsNotExist(err) {
					t.Fatal("fixture unexpectedly completed cleanup")
				}
			}
			if err := remove(); err != nil {
				t.Fatal("repeated Remove failed", err)
			}
			after, _ := os.ReadFile(result)
			if string(before) != string(after) {
				t.Fatal("repeated Remove invoked CNI after Destroy")
			}
		})
	}
}
