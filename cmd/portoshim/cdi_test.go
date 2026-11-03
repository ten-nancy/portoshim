package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testCDISpec = `
cdiVersion: "0.6.0"
kind: nvidia.com/gpu
devices:
- name: GPU-0
  containerEdits:
    deviceNodes:
    - path: /dev/nvidia0
- name: GPU-1
  containerEdits:
    deviceNodes:
    - path: /dev/nvidia1
containerEdits:
  env: [NVIDIA_CDI=1]
  deviceNodes:
  - path: /dev/nvidiactl
  mounts:
  - hostPath: /usr/lib/x86_64-linux-gnu/libcuda.so.590.48.01
    containerPath: /usr/lib/x86_64-linux-gnu/libcuda.so.1
    options: [ro, bind]
  hooks:
  - hookName: createContainer
    path: /usr/bin/nvidia-cdi-hook
    args: [nvidia-cdi-hook, update-ldcache, --folder, /usr/lib/x86_64-linux-gnu]
`

func writeTestCDISpec(t *testing.T) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nvidia.yaml"), []byte(testCDISpec), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCDIRequestedDevices(t *testing.T) {
	got := cdiRequestedDevices(map[string]string{
		"cdi.k8s.io/b": "nvidia.com/gpu=GPU-1",
		"cdi.k8s.io/a": "nvidia.com/gpu=GPU-0, nvidia.com/gpu=GPU-1",
		"other":        "nvidia.com/gpu=GPU-2",
	})
	want := []string{"nvidia.com/gpu=GPU-0", "nvidia.com/gpu=GPU-1", "nvidia.com/gpu=GPU-1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestResolveCDIDevices(t *testing.T) {
	dir := writeTestCDISpec(t)
	edits, err := resolveCDIDevices(context.Background(), []string{"nvidia.com/gpu=GPU-0", "nvidia.com/gpu=GPU-1", "nvidia.com/gpu=GPU-1"}, []string{dir})
	if err != nil {
		t.Fatal(err)
	}

	devices := []string{}
	for _, d := range edits.Devices {
		devices = append(devices, d.ContainerPath+":"+d.HostPath+":"+d.Permissions)
	}
	wantDevices := []string{"/dev/nvidiactl:/dev/nvidiactl:rwm", "/dev/nvidia0:/dev/nvidia0:rwm", "/dev/nvidia1:/dev/nvidia1:rwm"}
	if len(devices) != len(wantDevices) {
		t.Fatalf("devices %v, want %v", devices, wantDevices)
	}
	for i := range wantDevices {
		if devices[i] != wantDevices[i] {
			t.Fatalf("devices %v, want %v", devices, wantDevices)
		}
	}

	if len(edits.Mounts) != 1 || edits.Mounts[0].ContainerPath != "/usr/lib/x86_64-linux-gnu/libcuda.so.1" || !edits.Mounts[0].Readonly {
		t.Fatalf("mounts %+v", edits.Mounts)
	}
	if len(edits.Envs) != 1 || edits.Envs[0].Key != "NVIDIA_CDI" || edits.Envs[0].Value != "1" {
		t.Fatalf("envs %+v", edits.Envs)
	}
	if len(edits.Hooks) != 1 || edits.Hooks[0].HookName != "createContainer" || edits.Hooks[0].Args[1] != "update-ldcache" {
		t.Fatalf("hooks %+v", edits.Hooks)
	}
}

func TestRunCDIHooksNone(t *testing.T) {
	if err := runCDIHooks(context.Background(), "ct", "/nonexistent", nil); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCDIDevicesNoRequest(t *testing.T) {
	edits, err := resolveCDIDevices(context.Background(), nil, []string{"/nonexistent"})
	if err != nil || len(edits.Mounts)+len(edits.Devices)+len(edits.Envs) != 0 {
		t.Fatalf("edits %+v, err %v", edits, err)
	}
}

func TestResolveCDIDevicesUnknown(t *testing.T) {
	dir := writeTestCDISpec(t)
	for _, name := range []string{"nvidia.com/gpu=GPU-9", "vendor.com/x=0", "nvidia.com/gpu"} {
		if _, err := resolveCDIDevices(context.Background(), []string{name}, []string{dir}); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
}

func TestResolveInRoot(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"usr/lib/x86_64-linux-gnu", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"lib":                                   "usr/lib",
		"abs":                                   "/usr/lib",
		"loop":                                  "loop",
		"usr/lib/x86_64-linux-gnu/libcuda.so.1": "libcuda.so.590.48.01",
	}
	for link, dest := range links {
		if err := os.Symlink(dest, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}

	for path, want := range map[string]string{
		"/lib/firmware/nvidia/gsp.bin":            "/usr/lib/firmware/nvidia/gsp.bin",
		"/abs/x86_64-linux-gnu/libnvidia-ml.so.1": "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
		"/lib/x86_64-linux-gnu/libcuda.so.1":      "/usr/lib/x86_64-linux-gnu/libcuda.so.590.48.01",
		"/etc/../lib/../../usr/bin/nvidia-smi":    "/usr/bin/nvidia-smi",
		"/run/nvidia-persistenced/socket":         "/run/nvidia-persistenced/socket",
	} {
		got, err := resolveInRoot(root, path)
		if err != nil || got != want {
			t.Errorf("resolveInRoot(%s) = %s, %v; want %s", path, got, err, want)
		}
	}
	if _, err := resolveInRoot(root, "/loop/x"); err == nil {
		t.Error("symlink loop: no error")
	}
}

func TestCDIMountTarget(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"usr/lib/firmware", "usr/bin", "opt/wrapper/bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "opt/wrapper/bin/wrapper"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	for link, dest := range map[string]string{
		"lib":                         "usr/lib",
		"usr/bin/nvidia-smi":          "/opt/wrapper/bin/wrapper",
		"usr/bin/nvidia-persistenced": "/opt/wrapper/bin/wrapper",
	} {
		if err := os.Symlink(dest, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}

	for path, want := range map[string]string{
		"/lib/firmware/nvidia/gsp.bin":    "/usr/lib/firmware/nvidia/gsp.bin",
		"/usr/bin/nvidia-cuda-mps-server": "/usr/bin/nvidia-cuda-mps-server",
		"/usr/bin/nvidia-smi":             "",
		"/usr/bin/nvidia-persistenced":    "",
	} {
		got, err := cdiMountTarget(root, path)
		if err != nil || got != want {
			t.Errorf("cdiMountTarget(%s) = %q, %v; want %q", path, got, err, want)
		}
	}
}
