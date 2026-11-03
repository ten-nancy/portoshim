package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ten-nancy/porto/src/api/go/porto"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/cri-api/pkg/apis/runtime/v1"
	cdispecs "tags.cncf.io/container-device-interface/specs-go"
)

// Container Device Interface (https://github.com/cncf-tags/container-device-interface).
//
// Device plugins request CDI devices with container annotations
// "cdi.k8s.io/<plugin>" = "<vendor>/<class>=<name>[,...]"; the devices are described
// by spec files in Cfg.CDI.SpecDirs. Devices and envs of the container edits go with
// the CRI ones; mounts are linked into the created container by linkCDIMounts.
//
// Porto has no OCI hooks, so the hooks Portoshim can emulate are run by
// runCDIHooks between creating and starting the container: createRuntime,
// createContainer and prestart hooks get the OCI state of the container on
// stdin, with the bundle config pointing at the container root. Hooks that must
// run in the container namespaces (startContainer) and later ones are skipped.

const cdiAnnotationPrefix = "cdi.k8s.io/"

// cdiSpec is a parsed spec file.
type cdiSpec struct {
	cdispecs.Spec `yaml:",inline"`
	path          string
}

type cdiEdits struct {
	Mounts  []*v1.Mount
	Devices []*v1.Device
	Envs    []*v1.KeyValue
	Hooks   []*cdispecs.Hook
}

// cdiRequestedDevices returns the qualified CDI device names ("vendor/class=name")
// requested by the container annotations.
func cdiRequestedDevices(annotations map[string]string) []string {
	keys := make([]string, 0)
	for k := range annotations {
		if strings.HasPrefix(k, cdiAnnotationPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	devices := make([]string, 0)
	for _, k := range keys {
		for _, d := range strings.Split(annotations[k], ",") {
			if d = strings.TrimSpace(d); d != "" {
				devices = append(devices, d)
			}
		}
	}
	return devices
}

// loadCDISpecs reads the spec files; a later directory overrides an earlier one
// for the same kind, as in the CDI reference implementation.
func loadCDISpecs(dirs []string) (map[string]*cdiSpec, error) {
	specs := map[string]*cdiSpec{}
	for _, dir := range dirs {
		files, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		for _, f := range files {
			ext := filepath.Ext(f.Name())
			if f.IsDir() || (ext != ".yaml" && ext != ".json") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			spec := &cdiSpec{path: path}
			if err := yaml.Unmarshal(data, spec); err != nil {
				return nil, fmt.Errorf("CDI spec %s: %v", path, err)
			}
			if spec.Kind == "" {
				return nil, fmt.Errorf("CDI spec %s: no kind", path)
			}
			specs[spec.Kind] = spec
		}
	}
	return specs, nil
}

func (e *cdiEdits) apply(edits *cdispecs.ContainerEdits) {
	for _, env := range edits.Env {
		kv := strings.SplitN(env, "=", 2)
		if len(kv) != 2 {
			continue
		}
		e.Envs = append(e.Envs, &v1.KeyValue{Key: kv[0], Value: kv[1]})
	}
	for _, d := range edits.DeviceNodes {
		hostPath := d.HostPath
		if hostPath == "" {
			hostPath = d.Path
		}
		permissions := d.Permissions
		if permissions == "" {
			permissions = "rwm"
		}
		e.Devices = append(e.Devices, &v1.Device{ContainerPath: d.Path, HostPath: hostPath, Permissions: permissions})
	}
	for _, m := range edits.Mounts {
		readonly := false
		for _, o := range m.Options {
			if o == "ro" {
				readonly = true
			}
		}
		e.Mounts = append(e.Mounts, &v1.Mount{
			ContainerPath: m.ContainerPath,
			HostPath:      m.HostPath,
			Readonly:      readonly,
			Propagation:   v1.MountPropagation_PROPAGATION_PRIVATE,
		})
	}
	e.Hooks = append(e.Hooks, edits.Hooks...)
}

// resolveCDIDevices translates the requested CDI devices into container edits.
// An unknown device fails the container, as in containerd.
func resolveCDIDevices(ctx context.Context, requested []string, dirs []string) (*cdiEdits, error) {
	edits := &cdiEdits{}
	if len(requested) == 0 {
		return edits, nil
	}

	specs, err := loadCDISpecs(dirs)
	if err != nil {
		return nil, err
	}

	appliedSpecs := map[string]bool{}
	seen := map[string]bool{}
	for _, qualified := range requested {
		if seen[qualified] {
			continue
		}
		seen[qualified] = true

		kv := strings.SplitN(qualified, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid CDI device name %q", qualified)
		}
		spec, ok := specs[kv[0]]
		if !ok {
			return nil, fmt.Errorf("unresolvable CDI device %q: no spec for kind %q", qualified, kv[0])
		}
		var device *cdispecs.Device
		for i := range spec.Devices {
			if spec.Devices[i].Name == kv[1] {
				device = &spec.Devices[i]
				break
			}
		}
		if device == nil {
			return nil, fmt.Errorf("unresolvable CDI device %q: not in %s", qualified, spec.path)
		}

		if !appliedSpecs[spec.Kind] {
			appliedSpecs[spec.Kind] = true
			edits.apply(&spec.ContainerEdits)
		}
		edits.apply(&device.ContainerEdits)
	}
	return edits, nil
}

// resolveInRoot resolves path inside the container root through its symlinks,
// as runc does for mount targets (securejoin). The result is a path in the
// container; missing components are kept as they are.
func resolveInRoot(root string, path string) (string, error) {
	const maxSymlinks = 40
	symlinks := 0
	current := "/"
	parts := strings.Split(path, "/")
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, part)
		fi, err := os.Lstat(filepath.Join(root, next))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			current = next
			continue
		}
		if symlinks++; symlinks > maxSymlinks {
			return "", fmt.Errorf("too many symlinks in %s", path)
		}
		dest, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(dest) {
			current = "/"
		}
		parts = append(strings.Split(dest, "/"), parts...)
	}
	return current, nil
}

// cdiMountTarget returns where the CDI mount of path goes in the container
// root, or "" when the mount is to be skipped. The directories of the path are
// resolved through the symlinks of the root: Porto does not follow symlinks
// when it creates a link target, so in a merged-usr image a mount under /lib
// would put a directory over the /lib -> usr/lib symlink and hide the system
// libraries. A symlink at the path itself is left alone: the image put it there
// on purpose, e.g. /usr/bin/nvidia-smi -> its own wrapper, and several such
// links may lead to the same file.
func cdiMountTarget(root string, path string) (string, error) {
	path = filepath.Clean(path)
	dir, err := resolveInRoot(root, filepath.Dir(path))
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, filepath.Base(path))
	if fi, err := os.Lstat(filepath.Join(root, target)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", nil
	}
	return target, nil
}

// linkCDIMounts binds the CDI mounts into the created container.
func linkCDIMounts(ctx context.Context, pc porto.PortoAPI, id string, rootPath string, mounts []*v1.Mount) error {
	linked := map[string]bool{}
	for _, m := range mounts {
		target, err := cdiMountTarget(rootPath, m.ContainerPath)
		if err != nil {
			return fmt.Errorf("CDI mount %s: %v", m.ContainerPath, err)
		}
		if target == "" {
			WarnLog(ctx, "CDI mount %s: the image has a symlink there, skipped", m.ContainerPath)
			continue
		}
		if linked[target] {
			WarnLog(ctx, "CDI mount %s: %s is already mounted, skipped", m.ContainerPath, target)
			continue
		}
		linked[target] = true
		vol, err := pc.CreateVolume("", map[string]string{
			"backend":   "bind",
			"storage":   m.HostPath,
			"read_only": strconv.FormatBool(m.Readonly),
		})
		if err != nil {
			return fmt.Errorf("CDI mount %s: %v", m.ContainerPath, err)
		}
		err = pc.LinkVolumeTarget(vol.GetPath(), id, target, false, m.Readonly)
		// The volume is linked to its creator too; from now on the container owns it.
		if uerr := pc.UnlinkVolume(vol.GetPath(), ""); uerr != nil {
			WarnLog(ctx, "CDI mount %s: unlink volume %s: %v", m.ContainerPath, vol.GetPath(), uerr)
		}
		if err != nil {
			return fmt.Errorf("CDI mount %s -> %s: %v", m.ContainerPath, target, err)
		}
		DebugLog(ctx, "CDI mount %s -> %s (%s)", m.HostPath, target, vol.GetPath())
	}
	return nil
}

// cdiHookStages are the hooks run before the container starts, in OCI order.
var cdiHookStages = []string{"prestart", "createRuntime", "createContainer"}

const cdiHookDefaultTimeout = 60 * time.Second

// runCDIHooks runs the hooks of the created but not yet started container whose
// root is rootPath. Each hook runs in a private mount namespace: the hooks
// pivot_root into the container root, which fails on shared mounts.
func runCDIHooks(ctx context.Context, id string, rootPath string, hooks []*cdispecs.Hook) error {
	if len(hooks) == 0 {
		return nil
	}

	bundle, err := os.MkdirTemp("", "portoshim-cdi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bundle)

	config, _ := json.Marshal(map[string]interface{}{
		"ociVersion": "1.2.0",
		"root":       map[string]interface{}{"path": rootPath},
		"process":    map[string]interface{}{"cwd": "/"},
	})
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), config, 0o600); err != nil {
		return err
	}
	state, _ := json.Marshal(map[string]interface{}{
		"ociVersion": "1.2.0",
		"id":         id,
		"status":     "creating",
		"pid":        0,
		"bundle":     bundle,
	})

	for _, h := range hooks {
		known := false
		for _, s := range cdiHookStages {
			known = known || h.HookName == s
		}
		if !known {
			WarnLog(ctx, "CDI hook %s %v: stage %q is not supported, skipped", h.Path, h.Args, h.HookName)
		}
	}

	for _, stage := range cdiHookStages {
		for _, h := range hooks {
			if h.HookName != stage {
				continue
			}
			if err := runCDIHook(ctx, h, state); err != nil {
				return fmt.Errorf("CDI %s hook %s %v: %v", stage, h.Path, h.Args, err)
			}
		}
	}
	return nil
}

func runCDIHook(ctx context.Context, h *cdispecs.Hook, state []byte) error {
	timeout := cdiHookDefaultTimeout
	if h.Timeout != nil {
		timeout = time.Duration(*h.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, h.Path)
	cmd.Args = h.Args
	if len(cmd.Args) == 0 {
		cmd.Args = []string{h.Path}
	}
	cmd.Env = h.Env
	cmd.Stdin = bytes.NewReader(state)
	// With CLONE_NEWNS in Unshareflags the Go runtime also makes / rprivate.
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	out, err := cmd.CombinedOutput()
	DebugLog(ctx, "CDI hook %s %v: %s", h.Path, h.Args, out)
	if err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
