package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/pkg/netns"
	cni "github.com/containerd/go-cni"
	"github.com/ten-nancy/porto/src/api/go/porto"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
	"go.uber.org/zap"
	v1 "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming"
)

const (
	kubeResourceDomain = "portoshim.net"
	// loopback + default
	networkAttachCount     = 2
	ifPrefixName           = "veth"
	defaultIfName          = "veth0"
	maxSymlinkResolveDepth = 10
	defaultEnvPath         = "/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin"
)

var (
	podStateMap = map[string]v1.PodSandboxState{
		"stopped":    v1.PodSandboxState_SANDBOX_NOTREADY,
		"paused":     v1.PodSandboxState_SANDBOX_NOTREADY,
		"starting":   v1.PodSandboxState_SANDBOX_NOTREADY,
		"running":    v1.PodSandboxState_SANDBOX_READY,
		"stopping":   v1.PodSandboxState_SANDBOX_NOTREADY,
		"respawning": v1.PodSandboxState_SANDBOX_NOTREADY,
		"meta":       v1.PodSandboxState_SANDBOX_NOTREADY,
		"dead":       v1.PodSandboxState_SANDBOX_NOTREADY,
	}
	containerStateMap = map[string]v1.ContainerState{
		"stopped":    v1.ContainerState_CONTAINER_CREATED,
		"paused":     v1.ContainerState_CONTAINER_RUNNING,
		"starting":   v1.ContainerState_CONTAINER_RUNNING,
		"running":    v1.ContainerState_CONTAINER_RUNNING,
		"stopping":   v1.ContainerState_CONTAINER_RUNNING,
		"respawning": v1.ContainerState_CONTAINER_RUNNING,
		"meta":       v1.ContainerState_CONTAINER_RUNNING,
		"dead":       v1.ContainerState_CONTAINER_EXITED,
	}
)

var excludedMountSources = []string{"/dev", "/sys"}

type PortoshimRuntimeMapper struct {
	netPlugin       cni.CNI
	streamingServer streaming.Server
}

func NewPortoshimRuntimeMapper() (*PortoshimRuntimeMapper, error) {
	rm := &PortoshimRuntimeMapper{}
	netPlugin, err := cni.New(cni.WithMinNetworkCount(networkAttachCount),
		cni.WithPluginConfDir(Cfg.CNI.ConfDir),
		cni.WithPluginDir([]string{Cfg.CNI.BinDir}),
		cni.WithInterfacePrefix(ifPrefixName))
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cni: %v", err)
	}
	rm.netPlugin = netPlugin

	rm.streamingServer, err = NewStreamingServer(fmt.Sprintf("%s:%d", Cfg.StreamingServer.Address, Cfg.StreamingServer.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to create streaming server: %v", err)
	}

	go func() {
		err = rm.streamingServer.Start(true)
		if err != nil {
			zap.S().Warnf("failed to start streaming server: %v", err)
		}
	}()

	return rm, nil
}

// INTERNAL

// id
func createID(name string) string {
	length := 58
	if len(name) < length {
		length = len(name)
	}
	// max length of return value is 58 + 1 + 4 = 63, so container id <= 127
	return fmt.Sprintf("%s-%04x", name[:length], rand.Intn(65536))
}

func getPodAndContainer(id string) (string, string) {
	// <id> := <parentPortoContainer>/<podID>/<containerID>
	podID := ""

	idx := 0
	if len(Cfg.Porto.ParentContainer) != 0 && strings.HasPrefix(id, Cfg.Porto.ParentContainer) {
		idx = 1
	}
	parts := strings.Split(id, "/")
	if len(parts) > idx {
		podID = parts[idx]
	}
	idx = idx + 1
	containerID := ""
	if len(parts) > idx {
		containerID = parts[idx]
	}

	return podID, containerID
}

var (
	once      sync.Once
	parentCnt string
)

const (
	portoRoot = "/porto/"
)

func getParentCnt(ctx context.Context) string {
	once.Do(func() {
		var err error
		pc := getPortoClient(ctx)
		parentCnt, err = pc.GetProperty(Cfg.Porto.ParentContainer, "absolute_name")
		if err != nil {
			DebugLog(ctx, "Can't get property absolute_name: %v", err)
		}
		parentCnt = strings.TrimPrefix(parentCnt, portoRoot)
	})
	return parentCnt
}

func checkSlashCount(slashCount int, id, parentCnt string) bool {
	if len(parentCnt) != 0 && strings.HasPrefix(id, parentCnt) {
		portosCount := strings.Count(parentCnt, "/")
		slashCount = slashCount + portosCount + 1
	}
	for _, part := range strings.Split(id, "/") {
		if len(part) == 0 {
			return false
		}
	}

	return strings.Count(id, "/") == slashCount
}

// TODO take into account parent porto container
func isPod(id, parentCnt string) bool {
	return checkSlashCount(0, id, parentCnt)
}

func isContainer(id, parentCnt string) bool {
	return checkSlashCount(1, id, parentCnt)
}

// state converters
func convertPodState(state string) v1.PodSandboxState {
	if state, found := podStateMap[state]; found {
		return state
	}

	return v1.PodSandboxState_SANDBOX_NOTREADY
}

func convertContainerState(state string) v1.ContainerState {
	if state, found := containerStateMap[state]; found {
		return state
	}

	return v1.ContainerState_CONTAINER_UNKNOWN
}

// resources
func prepareResources(ctx context.Context, spec *pb.TContainerSpec, cfg *v1.LinuxContainerResources) error {
	if cfg == nil {
		return nil
	}

	// cpu
	cpuValue := float64(cfg.CpuQuota) / 100000
	spec.CpuLimit = &cpuValue
	spec.CpuGuarantee = &cpuValue

	// memory
	memoryValue := uint64(cfg.MemoryLimitInBytes)
	//spec.MemoryLimit = &memoryValue
	err := setPropertyByNameInPorto(ctx, spec, "MemoryLimit", &memoryValue)

	if err != nil {
		return fmt.Errorf("%v, can't set property  %s", err, "MemoryLimit")
	}
	DebugLog(ctx, "MemoryLimit: %v", spec.MemoryLimit)

	//spec.MemoryGuarantee = &memoryValue
	err = setPropertyByNameInPorto(ctx, spec, "MemoryGuarantee", &memoryValue)

	if err != nil {
		return fmt.Errorf("%v, can't set property  %s", err, "MemoryGuarantee")
	}
	return nil
}

// command and env
func wrapCmdWithLogShim(cmd []string) []string {
	// No logs needed for pause command
	if cmd[0] != "/pause" {
		cmd = append([]string{"/usr/sbin/logshim"}, cmd...)
	}
	return cmd
}

func isChrootPathExecutable(root, path string, depth int) (bool, error) {
	if depth > maxSymlinkResolveDepth {
		return false, fmt.Errorf("too many levels of symbolic links %d while maximum is %d", depth, maxSymlinkResolveDepth)
	}
	absPath := filepath.Join(root, path)
	fi, err := os.Lstat(absPath)
	if err != nil {
		return false, fmt.Errorf("failed to lstat path '%s' inside root '%s': %w", path, root, err)
	}
	if fi.Mode()&os.ModeSymlink > 0 {
		target, err := os.Readlink(absPath)
		if err != nil {
			return false, fmt.Errorf("failed to read link '%s' inside root '%s': %w", path, root, err)
		}
		if len(target) > 0 && target[0] != '/' {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return isChrootPathExecutable(root, target, depth+1)
	}
	return fi.Mode()&0o100 > 0 && !fi.IsDir(), nil
}

func findChrootExecutable(ctx context.Context, path, root, cmd string) string {
	var binaryPath string
	paths := strings.Split(path, ":")
	for _, p := range paths {
		candidate := filepath.Join(p, cmd)
		ok, err := isChrootPathExecutable(root, candidate, 1)
		DebugLog(ctx, "checking candidate path '%s' for cmd '%s' in root '%s', ok: %t, err: %v", candidate, cmd, root, ok, err)
		if ok {
			binaryPath = candidate
			break
		}
	}
	return binaryPath
}

func findEnvPathOrDefault(env []*pb.TContainerEnvVar) string {
	for _, e := range env {
		if *e.Name == "PATH" {
			return *e.Value
		}
	}
	return defaultEnvPath
}

func envToVars(ctx context.Context, env []string) []*pb.TContainerEnvVar {
	var envVars []*pb.TContainerEnvVar

	for _, v := range env {
		keyValue := strings.SplitN(v, "=", 2)
		if len(keyValue) < 2 {
			WarnLog(ctx, "skip environment variable parsing: %s", v)
			continue
		}

		envVars = append(envVars, &pb.TContainerEnvVar{
			Name:  &keyValue[0],
			Value: &keyValue[1],
		})
	}

	return envVars
}

func prepareCommand(ctx context.Context, spec *pb.TContainerSpec, cfgCmd, cfgArgs, imgCmd []string, disableLogShim bool) error {
	id := spec.GetName()
	env := spec.GetEnv().GetVar()

	cmd := imgCmd
	if len(cfgCmd) > 0 {
		cmd = cfgCmd
	}
	cmd = append(cmd, cfgArgs...)

	// Wrap non-absolute path command into call to /bin/sh -c
	if len(cmd) < 1 {
		return fmt.Errorf("got empty command for container %s", id)
	}
	if len(cmd[0]) < 1 {
		return fmt.Errorf("got malformed command '%v' for container %s", cmd, id)
	}
	// Try to find out binary path inside chroot if we have non-absolute command
	if cmd[0][0] != '/' {
		execPath := findChrootExecutable(ctx, findEnvPathOrDefault(env), getRootPath(id), cmd[0])
		if execPath != "" {
			cmd[0] = execPath
		} else {
			// Last resort, we failed to find out command, so let sh decide.
			cmd = append([]string{"/bin/sh", "-c"}, strings.Join(cmd, " "))
		}
	}

	if !disableLogShim {
		cmd = wrapCmdWithLogShim(cmd)
	}

	spec.CommandArgv = &pb.TContainerCommandArgv{
		Argv: cmd,
	}

	return nil
}

func prepareExecEnv(ctx context.Context, spec *pb.TContainerSpec, env string) {
	var envSlice []string
	if strings.Contains(env, "\\;") {
		// These actions for case when env has separator ";" in value of variable (e.g. "NAME1=value1;NAME2=some\;value2")
		specChar := string(rune(0x10FFFF))
		envSlice = strings.Split(strings.ReplaceAll(env, "\\;", specChar), ";")
		for i, s := range envSlice {
			envSlice[i] = strings.ReplaceAll(s, specChar, ";")
		}
	} else {
		envSlice = strings.Split(env, ";")
	}
	spec.Env = &pb.TContainerEnv{
		Var: envToVars(ctx, envSlice),
	}
}

func prepareEnv(ctx context.Context, spec *pb.TContainerSpec, env []*v1.KeyValue, image *pb.TDockerImage) {
	envVars := envToVars(ctx, image.GetConfig().GetEnv())

	for _, e := range env {
		envVars = append(envVars, &pb.TContainerEnvVar{
			Name:  &e.Key,
			Value: &e.Value,
		})
	}

	spec.Env = &pb.TContainerEnv{
		Var: envVars,
	}
}

// root and mounts
func sliceContainsString(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}

	return false
}

func getRootPath(id string) string {
	return filepath.Join(Cfg.Portoshim.VolumesDir, id)
}

func prepareRoot(ctx context.Context, spec *pb.TContainerSpec, volumes *[]*pb.TVolumeSpec, rootPath string, image string) error {
	id := spec.GetName()
	rootAbsPath := getRootPath(id)
	if rootPath == "" {
		rootPath = rootAbsPath
	}

	DebugLog(ctx, "mkdir: %s", rootAbsPath)
	err := os.MkdirAll(rootAbsPath, 0755)
	if err != nil {
		if os.IsExist(err) {
			WarnLog(ctx, "%s: directory already exists: %s", getCurrentFuncName(), rootPath)
		} else {
			return fmt.Errorf("%s: %v", getCurrentFuncName(), err)
		}
	}
	root := pb.TVolumeSpec{
		Links: []*pb.TVolumeLink{&pb.TVolumeLink{
			Container: &id,
			Target:    getStringPointer("/"),
		}},
		Backend: getStringPointer("overlay"),
		Place:   &Cfg.Images.Place,
		Image:   &image,
	}

	*volumes = append(*volumes, &root)
	spec.Root = &rootPath

	return nil
}

func convertMountToVolumeSpec(id string, mount *v1.Mount) *pb.TVolumeSpec {
	return &pb.TVolumeSpec{
		Links: []*pb.TVolumeLink{&pb.TVolumeLink{
			Container: &id,
			Target:    &mount.ContainerPath,
			ReadOnly:  &mount.Readonly,
		}},
		Backend:  getStringPointer("bind"),
		Storage:  &mount.HostPath,
		ReadOnly: &mount.Readonly,
	}
}

func prepareContainerMounts(ctx context.Context, id string, volumes *[]*pb.TVolumeSpec, mounts []*v1.Mount) {
	// Mount logshim binary to container
	mounts = append(mounts,
		&v1.Mount{
			ContainerPath: "/usr/sbin/logshim",
			HostPath:      "/usr/sbin/logshim",
			Readonly:      true,
			Propagation:   v1.MountPropagation_PROPAGATION_PRIVATE,
		})

	for _, mount := range mounts {
		// pre-normalize volume path for porto as it expects "normal" path
		mount.ContainerPath = filepath.Clean(mount.ContainerPath)
		mount.HostPath = filepath.Clean(mount.HostPath)
		if sliceContainsString(excludedMountSources, mount.HostPath) {
			continue
		}

		// TODO: durty hack
		if mount.ContainerPath == "/var/run/secrets/kubernetes.io/serviceaccount" {
			retry(ctx, func() error {
				_, err := os.Stat(mount.HostPath + "/ca.crt")
				if err == nil {
					return nil
				}
				return fmt.Errorf("%s waiting for a %s", id, mount.HostPath)
			}, func() int {
				return 1000
			}, 3)
		}
		*volumes = append(*volumes, convertMountToVolumeSpec(id, mount))
	}
	sort.Slice(*volumes, func(i, j int) bool {
		return *((*volumes)[i].Links[0].Target) < *((*volumes)[j].Links[0].Target)
	})

}

// labels and annotations
func convertBase64(src string, encode bool) string {
	if encode {
		return base64.RawStdEncoding.EncodeToString([]byte(src))
	}

	dst, err := base64.RawStdEncoding.DecodeString(src)
	if err != nil {
		return src
	}

	return string(dst)
}

func encodeLabel(src string, prefix string) string {
	dst := convertBase64(src, true)
	if prefix != "" {
		dst = prefix + "." + dst
	}

	return dst
}

func decodeLabel(src string, prefix string) string {
	dst := src
	if prefix != "" {
		dst = strings.TrimPrefix(dst, prefix+".")
	}

	return convertBase64(dst, false)
}

func convertToStringMap(m map[string]string) *pb.TStringMap {
	sm := []*pb.TStringMap_TStringMapEntry{}

	for k, v := range m {
		sm = append(sm, &pb.TStringMap_TStringMapEntry{
			Key: getStringPointer(k),
			Val: getStringPointer(v),
		})
	}

	return &pb.TStringMap{
		Map: sm,
	}
}

func convertToPortoLabels(labels map[string]string, annotations map[string]string) map[string]string {
	portoLabels := make(map[string]string)
	for label, value := range labels {
		portoLabels[encodeLabel(label, "LABEL")] = encodeLabel(value, "")
	}

	for annotation, value := range annotations {
		portoLabels[encodeLabel(annotation, "ANNOTATION")] = encodeLabel(value, "")
	}

	return portoLabels
}

func convertFromPortoLabels(portoLabels string) (map[string]string, map[string]string) {
	labels := make(map[string]string)
	annotations := make(map[string]string)

	if len(portoLabels) <= 0 {
		return map[string]string{}, map[string]string{}
	}

	// porto labels parsing
	for _, pair := range strings.Split(portoLabels, ";") {
		splitedPair := strings.Split(pair, ":")
		rawLabel := strings.TrimSpace(splitedPair[0])
		rawValue := strings.TrimSpace(splitedPair[1])
		if strings.HasPrefix(rawLabel, "LABEL") {
			labels[decodeLabel(rawLabel, "LABEL")] = decodeLabel(rawValue, "")
		} else if strings.HasPrefix(rawLabel, "ANNOTATION") {
			annotations[decodeLabel(rawLabel, "ANNOTATION")] = decodeLabel(rawValue, "")
		}

	}

	return labels, annotations
}

func preparePodLabels(podSpec *pb.TContainerSpec, cfg *v1.PodSandboxConfig) {
	id := podSpec.GetName()
	labels := cfg.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	if _, found := labels["io.kubernetes.pod.name"]; !found {
		if name := cfg.GetMetadata().GetName(); name != "" {
			labels["io.kubernetes.pod.name"] = name
		}
	}
	if _, found := labels["io.kubernetes.pod.uid"]; !found {
		if uid := cfg.GetMetadata().GetUid(); uid != "" {
			labels["io.kubernetes.pod.uid"] = uid
		}
	}
	if _, found := labels["io.kubernetes.pod.namespace"]; !found {
		if ns := cfg.GetMetadata().GetNamespace(); ns != "" {
			labels["io.kubernetes.pod.namespace"] = ns
		}
	}
	labels["attempt"] = fmt.Sprint(cfg.GetMetadata().GetAttempt())
	labels["portoshim.pod.id"] = id
	labels["portoshim.pod.image"] = Cfg.Images.PauseImage

	portoLabels := convertToPortoLabels(labels, cfg.GetAnnotations())
	portoLabels["INFRA.engine"] = "k8s"
	podSpec.Labels = convertToStringMap(portoLabels)
}

func prepareContainerLabels(containerSpec *pb.TContainerSpec, cfg *v1.ContainerConfig, image string) {
	id := containerSpec.GetName()
	labels := cfg.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	if _, found := labels["io.kubernetes.container.name"]; !found {
		if name := cfg.GetMetadata().GetName(); name != "" {
			labels["io.kubernetes.container.name"] = name
		}
	}
	labels["attempt"] = fmt.Sprint(cfg.GetMetadata().GetAttempt())
	labels["io.kubernetes.container.logpath"] = filepath.Join("/place/porto/", id, "/stdout")
	labels["portoshim.container.id"] = id
	labels["portoshim.container.image"] = image

	portoLabels := convertToPortoLabels(labels, cfg.GetAnnotations())
	portoLabels["INFRA.engine"] = "k8s"
	containerSpec.Labels = convertToStringMap(portoLabels)
}

// getters and converters
func getStringPointer(v string) *string {
	return &v
}

func getBoolPointer(v bool) *bool {
	return &v
}

func getUintPointer(v uint64) *uint64 {
	return &v
}

func convertValueToTime(value string) int64 {
	return time.Unix(convertValueToInt(value), 0).UnixNano()
}

func convertValueToUint(value string) uint64 {
	res, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}

	return res
}

func convertValueToInt(value string) int64 {
	res, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}

	return res
}

func convertPodMetadata(labels map[string]string) *v1.PodSandboxMetadata {
	attempt, _ := strconv.ParseUint(labels["attempt"], 10, 64)

	return &v1.PodSandboxMetadata{
		Name:      labels["io.kubernetes.pod.name"],
		Uid:       labels["io.kubernetes.pod.uid"],
		Namespace: labels["io.kubernetes.pod.namespace"],
		Attempt:   uint32(attempt),
	}
}

func convertContainerMetadata(labels map[string]string) *v1.ContainerMetadata {
	attempt, _ := strconv.ParseUint(labels["attempt"], 10, 64)

	return &v1.ContainerMetadata{
		Name:    labels["io.kubernetes.container.name"],
		Attempt: uint32(attempt),
	}
}

func getProperties(ctx context.Context, id string, names []string) (map[string]string, error) {
	pc := getPortoClient(ctx)

	rsp, err := pc.Get([]string{id}, names)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	properties := make(map[string]string)
	for _, item := range rsp.GetList() {
		if item.GetName() != id {
			continue
		}
		for _, value := range item.GetKeyval() {
			if value.GetError() == pb.EError_ContainerDoesNotExist {
				return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), value.GetErrorMsg())
			}
			properties[value.GetVariable()] = value.GetValue()
		}
	}

	return properties, nil
}

func getContainerImage(ctx context.Context, id string, labels map[string]string) string {
	if image, found := labels["portoshim.container.image"]; found {
		return image
	}

	if len(labels) <= 0 {
		return ""
	}

	// try to get image from root volume
	pc := getPortoClient(ctx)
	imageDescriptions, err := pc.ListVolumes(getRootPath(id), id)
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
		return ""
	}

	return imageDescriptions[0].Properties["image"]
}

func getPodStats(ctx context.Context, id, portoid string) *v1.PodSandboxStats {
	timestamp := time.Now().UnixNano()
	pc := getPortoClient(ctx)

	response, err := pc.ListContainers(portoid + "/*")
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
		return nil
	}

	var stats []*v1.ContainerStats
	for _, portoCtrID := range response {
		id := removePortoPrefix(ctx, portoCtrID)
		stats = append(stats, getContainerStats(ctx, id, portoCtrID))
	}

	props, err := getProperties(ctx, portoid, []string{"labels", "cpu_usage", "memory_usage", "minor_faults", "major_faults", "net_rx_bytes", "net_bytes", "process_count"})
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
		return nil
	}

	labels, annotations := convertFromPortoLabels(props["labels"])
	cpu := convertValueToUint(props["cpu_usage"])

	// TODO: Заполнить оставшиеся метрики
	return &v1.PodSandboxStats{
		Attributes: &v1.PodSandboxAttributes{
			Id:          id,
			Metadata:    convertPodMetadata(labels),
			Labels:      labels,
			Annotations: annotations,
		},
		Linux: &v1.LinuxPodSandboxStats{
			Cpu: &v1.CpuUsage{
				Timestamp:            timestamp,
				UsageCoreNanoSeconds: &v1.UInt64Value{Value: cpu},
				UsageNanoCores:       &v1.UInt64Value{Value: cpu / 1000000000},
			},
			Memory: &v1.MemoryUsage{
				Timestamp:       timestamp,
				WorkingSetBytes: &v1.UInt64Value{Value: 0},
				AvailableBytes:  &v1.UInt64Value{Value: 0},
				UsageBytes:      &v1.UInt64Value{Value: convertValueToUint(props["memory_usage"])},
				RssBytes:        &v1.UInt64Value{Value: 0},
				PageFaults:      &v1.UInt64Value{Value: convertValueToUint(props["minor_faults"])},
				MajorPageFaults: &v1.UInt64Value{Value: convertValueToUint(props["major_faults"])},
			},
			Network: &v1.NetworkUsage{
				Timestamp: timestamp,
				DefaultInterface: &v1.NetworkInterfaceUsage{
					Name:     defaultIfName,
					RxBytes:  &v1.UInt64Value{Value: convertValueToUint(props["net_rx_bytes"])},
					RxErrors: &v1.UInt64Value{Value: 0},
					TxBytes:  &v1.UInt64Value{Value: convertValueToUint(props["net_bytes"])},
					TxErrors: &v1.UInt64Value{Value: 0},
				},
			},
			Process: &v1.ProcessUsage{
				Timestamp:    timestamp,
				ProcessCount: &v1.UInt64Value{Value: convertValueToUint(props["process_count"])},
			},
			Containers: stats,
		},
		Windows: &v1.WindowsPodSandboxStats{},
	}
}

func getContainerStats(ctx context.Context, id, portoid string) *v1.ContainerStats {
	timestamp := time.Now().UnixNano()
	props, err := getProperties(ctx, portoid, []string{"labels", "cpu_usage", "memory_usage", "minor_faults", "major_faults"})
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
		return nil
	}
	labels, annotations := convertFromPortoLabels(props["labels"])
	cpu := convertValueToUint(props["cpu_usage"])

	// TODO: Заполнить оставшиеся метрики
	return &v1.ContainerStats{
		Attributes: &v1.ContainerAttributes{
			Id:          id,
			Metadata:    convertContainerMetadata(labels),
			Labels:      labels,
			Annotations: annotations,
		},
		Cpu: &v1.CpuUsage{
			Timestamp:            timestamp,
			UsageCoreNanoSeconds: &v1.UInt64Value{Value: cpu},
			UsageNanoCores:       &v1.UInt64Value{Value: cpu / 1000000000},
		},
		Memory: &v1.MemoryUsage{
			Timestamp:       timestamp,
			WorkingSetBytes: &v1.UInt64Value{Value: 0},
			AvailableBytes:  &v1.UInt64Value{Value: 0},
			UsageBytes:      &v1.UInt64Value{Value: convertValueToUint(props["memory_usage"])},
			RssBytes:        &v1.UInt64Value{Value: 0},
			PageFaults:      &v1.UInt64Value{Value: convertValueToUint(props["minor_faults"])},
			MajorPageFaults: &v1.UInt64Value{Value: convertValueToUint(props["major_faults"])},
		},
		WritableLayer: &v1.FilesystemUsage{
			Timestamp: timestamp,
			FsId: &v1.FilesystemIdentifier{
				Mountpoint: getRootPath(id),
			},
			UsedBytes:  &v1.UInt64Value{Value: 0},
			InodesUsed: &v1.UInt64Value{Value: 0},
		},
	}
}

// network
func prepareContainerResolvConf(containerSpec *pb.TContainerSpec, cfg *v1.DNSConfig) {
	if cfg == nil {
		return
	}

	resolvConf := []string{}
	for _, i := range cfg.GetServers() {
		resolvConf = append(resolvConf, fmt.Sprintf("%s %s", "nameserver", i))
	}
	resolvConf = append(resolvConf, fmt.Sprintf("%s %s", "search", strings.Join(cfg.GetSearches(), " ")))
	resolvConf = append(resolvConf, fmt.Sprintf("%s %s\n", "options", strings.Join(cfg.GetOptions(), " ")))
	containerSpec.ResolvConf = getStringPointer(strings.Join(resolvConf, ";"))
}

func convertContainerNetNsMode(net string) v1.NamespaceMode {
	netNSMode := v1.NamespaceMode_NODE
	netNSProp := parsePropertyNetNS(net)
	if netNSProp != "" {
		netNSMode = v1.NamespaceMode_POD
	}

	return netNSMode
}

func convertPodSandboxNetworkStatus(addresses string) *v1.PodSandboxNetworkStatus {
	ips := []*v1.PodIP{}
	if len(addresses) > 0 {
		for _, address := range strings.Split(addresses, ";") {
			if pair := strings.Split(address, " "); len(pair) > 1 {
				if ip := pair[1]; ip != "auto" {
					ips = append(ips, &v1.PodIP{Ip: ip})
				}
			}
		}
	}

	var status v1.PodSandboxNetworkStatus
	if len(ips) > 0 {
		status.Ip = ips[0].GetIp()
	}
	if len(ips) > 1 {
		status.AdditionalIps = ips[1:]
	}

	return &status
}

func parsePropertyNetNS(prop string) string {
	netNSL := strings.Fields(prop)
	if netNSL[0] == "netns" && len(netNSL) > 1 {
		return netNSL[1]
	}
	return ""
}

func convertToCNILabels(id string, config *v1.PodSandboxConfig) map[string]string {
	return map[string]string{
		"K8S_POD_NAMESPACE":          config.GetMetadata().GetNamespace(),
		"K8S_POD_NAME":               config.GetMetadata().GetName(),
		"K8S_POD_INFRA_CONTAINER_ID": id,
		"K8S_POD_UID":                config.GetMetadata().GetUid(),
		"IgnoreUnknown":              "1",
	}
}

func (m *PortoshimRuntimeMapper) preparePodNetwork(ctx context.Context, podSpec *pb.TContainerSpec, cfg *v1.PodSandboxConfig) error {
	id := podSpec.GetName()

	if nsOpts := cfg.GetLinux().GetSecurityContext().GetNamespaceOptions(); nsOpts == nil || nsOpts.GetNetwork() == v1.NamespaceMode_NODE {
		return nil
	}

	if m.netPlugin == nil {
		return fmt.Errorf("cni wasn't initialized")
	}

	if err := m.netPlugin.Load(cni.WithLoNetwork, cni.WithDefaultConf); err != nil {
		return fmt.Errorf("failed to load cni configuration: %v", err)
	}

	netnsPath, err := netns.NewNetNS(Cfg.CNI.NetnsDir)
	if err != nil {
		return fmt.Errorf("failed to create network namespace, pod %s: %w", id, err)
	}

	DebugLog(ctx, "NewNetNS created: %s", netnsPath.GetPath())
	portoid := removePortoPrefix(ctx, id)
	cniNSOpts := []cni.NamespaceOpts{
		cni.WithCapability("io.kubernetes.cri.pod-annotations", cfg.Annotations),
		cni.WithLabels(convertToCNILabels(portoid, cfg)),
	}
	result, err := m.netPlugin.Setup(ctx, portoid, netnsPath.GetPath(), cniNSOpts...)
	if err != nil {
		DebugLog(ctx, "Failed to setup: %v", portoid)
		return err
	}

	resultJson, _ := json.Marshal(result)
	DebugLog(ctx, "netPlugin result: %v", string(resultJson))

	// hostname
	podSpec.Hostname = getStringPointer(cfg.GetHostname())

	// net
	podSpec.Net = &pb.TContainerNetConfig{
		Cfg: []*pb.TContainerNetOption{
			&pb.TContainerNetOption{
				Opt: getStringPointer("netns"),
				Arg: []string{filepath.Base(netnsPath.GetPath())},
			},
		},
	}

	// ip
	addrs := []*pb.TContainerIpConfig_TContainerIp{}
	for _, ip := range result.Interfaces[defaultIfName].IPConfigs {
		ipv6 := ip.IP.To16()
		if ipv6 != nil {
			addrs = append(addrs, &pb.TContainerIpConfig_TContainerIp{
				Dev: getStringPointer(defaultIfName),
				Ip:  getStringPointer(ipv6.String()),
			})
		}
	}
	podSpec.Ip = &pb.TContainerIpConfig{
		Cfg: addrs,
	}
	addrJson, _ := json.Marshal(addrs)
	DebugLog(ctx, "addrs: %v", string(addrJson))

	// sysctl
	podSpec.Sysctl = convertToStringMap(cfg.GetLinux().GetSysctls())

	// tx and rx limits
	for k, v := range cfg.GetAnnotations() {
		if strings.HasPrefix(k, "net-") {
			limit, _ := strconv.ParseUint(v, 0, 64)
			m := &pb.TUintMap{
				Map: []*pb.TUintMap_TUintMapEntry{
					&pb.TUintMap_TUintMapEntry{
						Key: getStringPointer(ifPrefixName),
						Val: getUintPointer(limit),
					},
				},
			}
			if k == filepath.Join(kubeResourceDomain, "net-tx") {
				podSpec.NetLimit = m
			} else if k == filepath.Join(kubeResourceDomain, "net-rx") {
				podSpec.NetRxLimit = m
			}
		}
	}

	return nil
}

func prepareContainerDevices(containerSpec *pb.TContainerSpec, devices []*v1.Device) {
	portoDevices := &pb.TContainerDevices{}
	for _, device := range devices {
		portoDevice := &pb.TContainerDevice{
			Device: &device.HostPath,
			Access: &device.Permissions,
			Path:   &device.ContainerPath,
		}
		portoDevices.Device = append(portoDevices.Device, portoDevice)
	}
	containerSpec.Devices = portoDevices
}

// RUNTIME SERVICE INTERFACE

func (m *PortoshimRuntimeMapper) Version(ctx context.Context, req *v1.VersionRequest) (*v1.VersionResponse, error) {
	pc := getPortoClient(ctx)

	tag, _, err := pc.GetVersion()
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}
	// TODO: temprorary use tag as a RuntimeApiVersion
	return &v1.VersionResponse{
		Version:           req.GetVersion(),
		RuntimeName:       Cfg.Porto.RuntimeName,
		RuntimeVersion:    tag,
		RuntimeApiVersion: tag,
	}, nil
}

func addPortoPrefix(ctx context.Context, id *string) *string {
	if id == nil {
		return nil
	}

	portoContainerName := getParentCnt(ctx)
	var result string
	if len(portoContainerName) != 0 {
		result = filepath.Join(portoContainerName, *id)
		// container name could not start with /
		// See TContainer::ValidName
		// https://github.com/ten-nancy/porto/blob/main/src/container.cpp#L106
		result = strings.TrimPrefix(result, "/")
	} else {
		result = *id
	}
	return &result
}

func removePortoPrefix(ctx context.Context, id string) string {
	portoContainerName := getParentCnt(ctx)
	if len(portoContainerName) != 0 && strings.HasPrefix(id, portoContainerName) {
		return id[len(portoContainerName)+1:]
	}
	return id
}

func (m *PortoshimRuntimeMapper) RunPodSandbox(ctx context.Context, req *v1.RunPodSandboxRequest) (*v1.RunPodSandboxResponse, error) {
	id := createID(req.GetConfig().GetMetadata().GetName())
	pc := getPortoClient(ctx)
	portoid := *addPortoPrefix(ctx, &id)
	var (
		err   error
		image *pb.TDockerImage
	)

	// get image
	DebugLog(ctx, "check image: %s", Cfg.Images.PauseImage)
	retry(ctx, func() error {
		image, err = pc.DockerImageStatus(Cfg.Images.PauseImage, Cfg.Images.Place)
		if err == nil {
			return nil
		}
		DebugLog(ctx, "No image %s need to pull it", Cfg.Images.PauseImage)

		registry := GetImageRegistry(Cfg.Images.PauseImage)
		authToken := registry.AuthToken

		registryCreds := porto.DockerRegistryCredentials{
			AuthToken: authToken,
		}

		image, err = pc.PullDockerImage(porto.DockerImage{
			Name:  Cfg.Images.PauseImage,
			Place: Cfg.Images.Place,
		}, registryCreds)
		if err != nil {
			return fmt.Errorf("Failed to pull docker image %s: %v", Cfg.Images.PauseImage, err)
		}
		return nil
	}, func() int {
		return 1000
	}, 3)

	if err != nil {
		return nil, fmt.Errorf("No image %v", err)
	}

	// specs initialization for CreateFromSpec
	podSpec := &pb.TContainerSpec{
		Name: &portoid,
	}
	volumes := &[]*pb.TVolumeSpec{}

	// resources
	if res := req.GetConfig().GetLinux().GetResources(); res != nil {
		DebugLog(ctx, "prepare resources: %+v", res)
		if err = prepareResources(ctx, podSpec, res); err != nil {
			return nil, fmt.Errorf("%v Failed to prepare resources", err)
		}
	}

	// labels and annotations
	DebugLog(ctx,
		"prepare labels and annotations: labels=%+v annotations=%+v",
		req.GetConfig().GetLabels(),
		req.GetConfig().GetAnnotations(),
	)
	preparePodLabels(podSpec, req.GetConfig())

	// root
	DebugLog(ctx, "prepare pod root: %s %s", portoid, Cfg.Images.PauseImage)
	if err := prepareRoot(ctx, podSpec, volumes, "", Cfg.Images.PauseImage); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// create pod and rootfs
	DebugLog(ctx, "create pod from spec: %+v", portoid)
	if err = pc.CreateFromSpec(podSpec, *volumes, false); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// spec reinitialization for UpdateFromSpec
	podSpec = &pb.TContainerSpec{
		Name: &portoid,
	}

	// Container prepare order is mandatory!
	// env and command MUST be prepared ONLY AFTER container chroot ready
	DebugLog(ctx, "prepare pod environment variables: %+v", image.GetConfig().GetEnv())
	prepareEnv(ctx, podSpec, []*v1.KeyValue{}, image)

	// command + args
	DebugLog(ctx, "prepare pod command: cmd=%+v", image.GetConfig().GetCmd())
	if err = prepareCommand(ctx, podSpec, []string{}, []string{}, image.GetConfig().GetCmd(), false); err != nil {
		_ = pc.Destroy(portoid)
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// network
	DebugLog(ctx, "prepare pod network: %+v", req.GetConfig())
	if err = m.preparePodNetwork(ctx, podSpec, req.GetConfig()); err != nil {
		DebugLog(ctx, "failed to preparePodNetwork: %v", err)
		_ = pc.Destroy(portoid)
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// set network, command and environment variables
	DebugLog(ctx, "update and start pod from spec: %+v", &portoid)
	podSpecJson, _ := json.Marshal(podSpec)
	DebugLog(ctx, "podspec: %v", string(podSpecJson))
	if err = pc.UpdateFromSpec(podSpec, true); err != nil {
		DebugLog(ctx, "Failed to UpdateFromSpec %v", err)
		_ = pc.Destroy(portoid)
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	return &v1.RunPodSandboxResponse{
		PodSandboxId: id,
	}, nil
}

func (m *PortoshimRuntimeMapper) StopPodSandbox(ctx context.Context, req *v1.StopPodSandboxRequest) (*v1.StopPodSandboxResponse, error) {
	pc := getPortoClient(ctx)

	id := req.GetPodSandboxId()
	parentCnt := getParentCnt(ctx)
	if !isPod(id, parentCnt) {
		return nil, fmt.Errorf("%s: %s specified ID belongs to container", getCurrentFuncName(), id)
	}

	portoid := *addPortoPrefix(ctx, &id)

	// pod existing
	state, err := pc.GetProperty(portoid, "state")
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
	}

	if state == "running" {
		if err = pc.Kill(portoid, 15); err != nil {
			return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
		}
	}

	return &v1.StopPodSandboxResponse{}, nil
}

func (m *PortoshimRuntimeMapper) RemovePodSandbox(ctx context.Context, req *v1.RemovePodSandboxRequest) (*v1.RemovePodSandboxResponse, error) {
	pc := getPortoClient(ctx)

	id := req.GetPodSandboxId()
	parentCnt := getParentCnt(ctx)
	if !isPod(id, parentCnt) {
		return nil, fmt.Errorf("%s: %s specified ID belongs to container", getCurrentFuncName(), id)
	}
	portoid := *addPortoPrefix(ctx, &id)

	DebugLog(ctx, "RemovePodSandbox: %v", id)

	netProp, err := pc.GetProperty(portoid, "net")
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	if err := pc.Destroy(portoid); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	rootPath := getRootPath(portoid)
	if err := os.RemoveAll(rootPath); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// removes the network from the pod
	netnsProp := parsePropertyNetNS(netProp)
	if netnsProp != "" {
		netnsPath := netns.LoadNetNS(filepath.Join(Cfg.CNI.NetnsDir, netnsProp))
		if err := m.netPlugin.Remove(ctx, portoid, netnsPath.GetPath(), cni.WithLabels(map[string]string{})); err != nil {
			return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
		}
		if err := netnsPath.Remove(); err != nil {
			return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
		}
	}

	return &v1.RemovePodSandboxResponse{}, nil
}

func (m *PortoshimRuntimeMapper) PodSandboxStatus(ctx context.Context, req *v1.PodSandboxStatusRequest) (*v1.PodSandboxStatusResponse, error) {
	id := req.GetPodSandboxId()
	parentCnt := getParentCnt(ctx)
	if !isPod(id, parentCnt) {
		return nil, fmt.Errorf("%s: %s specified ID belongs to container", getCurrentFuncName(), id)
	}

	DebugLog(ctx, "PodSandboxStatus: %s", id)
	portoid := *addPortoPrefix(ctx, &id)
	props, err := getProperties(ctx, portoid, []string{"labels", "state", "creation_time[raw]", "ip", "net"})
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	DebugLog(ctx, "props: %v", props)
	labels, annotations := convertFromPortoLabels(props["labels"])

	resp := &v1.PodSandboxStatusResponse{
		Status: &v1.PodSandboxStatus{
			Id:        id,
			Metadata:  convertPodMetadata(labels),
			State:     convertPodState(props["state"]),
			CreatedAt: convertValueToTime(props["creation_time[raw]"]),
			Network:   convertPodSandboxNetworkStatus(props["ip"]),
			Linux: &v1.LinuxPodSandboxStatus{
				Namespaces: &v1.Namespace{
					Options: &v1.NamespaceOption{
						Network: convertContainerNetNsMode(props["net"]),
						Pid:     v1.NamespaceMode_POD,
						Ipc:     v1.NamespaceMode_POD,
					},
				},
			},
			Labels:      labels,
			Annotations: annotations,
		},
	}

	return resp, nil
}

func (m *PortoshimRuntimeMapper) PodSandboxStats(ctx context.Context, req *v1.PodSandboxStatsRequest) (*v1.PodSandboxStatsResponse, error) {
	id := req.GetPodSandboxId()

	DebugLog(ctx, "PodSandboxStats id: %v", id)
	portoid := *addPortoPrefix(ctx, &id)
	return &v1.PodSandboxStatsResponse{
		Stats: getPodStats(ctx, id, portoid),
	}, nil
}

func (m *PortoshimRuntimeMapper) ListPodSandbox(ctx context.Context, req *v1.ListPodSandboxRequest) (*v1.ListPodSandboxResponse, error) {
	pc := getPortoClient(ctx)

	targetID := req.GetFilter().GetId()
	targetState := req.GetFilter().GetState()
	targetLabels := req.GetFilter().GetLabelSelector()

	// TODO(pau) request only pods
	mask := ""
	if targetID != "" {
		mask = targetID
	}

	response, err := pc.ListContainers(mask)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	DebugLog(ctx, "pc.ListContainers(%v) returns %+v", mask, response)

	var items []*v1.PodSandbox
	parentCnt := getParentCnt(ctx)
	for _, id := range response {
		if !isPod(id, parentCnt) {
			DebugLog(ctx, "not a pod %v, parent %v", id, parentCnt)
			continue
		}
		props, err := getProperties(ctx, id, []string{"labels", "state", "creation_time[raw]"})
		if err != nil {
			WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
			continue
		}
		labels, annotations := convertFromPortoLabels(props["labels"])

		// skip not k8s
		if _, found := labels["portoshim.pod.id"]; !found {
			// backward compatibility
			if _, found := labels["io.kubernetes.pod.namespace"]; !found {
				continue
			}
		}

		// filtering
		state := convertPodState(props["state"])
		if targetState != nil && targetState.GetState() != state {
			continue
		}

		skip := false
		for targetLabel, targetValue := range targetLabels {
			if value, found := labels[targetLabel]; !found || value != targetValue {
				skip = true
				break
			}
		}

		if skip {
			continue
		}

		items = append(items, &v1.PodSandbox{
			Id:          removePortoPrefix(ctx, id),
			Metadata:    convertPodMetadata(labels),
			State:       state,
			CreatedAt:   convertValueToTime(props["creation_time[raw]"]),
			Labels:      labels,
			Annotations: annotations,
		})
	}

	return &v1.ListPodSandboxResponse{
		Items: items,
	}, nil
}

func (m *PortoshimRuntimeMapper) CreateContainer(ctx context.Context, req *v1.CreateContainerRequest) (*v1.CreateContainerResponse, error) {
	// <id> := <parentPortoContainer>/<podID>/<containerID>
	podID := req.GetPodSandboxId()
	DebugLog(ctx, "CreateContainer: podID %s, %s", podID, req.PodSandboxId)
	podID = *addPortoPrefix(ctx, &podID)
	containerID := createID(req.GetConfig().GetMetadata().GetName())
	id := filepath.Join(podID, containerID)
	DebugLog(ctx, "CreateContainer: containerID %s", containerID)
	DebugLog(ctx, "CreateContainer: id %s", id)
	imageName := req.GetConfig().GetImage().GetImage()
	pc := getPortoClient(ctx)

	// get image
	DebugLog(ctx, "check image: %s", imageName)
	image, err := pc.DockerImageStatus(imageName, Cfg.Images.Place)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// specs initialization for CreateFromSpec
	containerSpec := &pb.TContainerSpec{
		Name: &id,
	}
	volumes := &[]*pb.TVolumeSpec{}

	// resources
	if res := req.GetConfig().GetLinux().GetResources(); res != nil {
		DebugLog(ctx, "prepare resources: %+v", res)
		if err = prepareResources(ctx, containerSpec, res); err != nil {
			return nil, fmt.Errorf("%v Failed to prepare resources", err)
		}
	}

	// labels and annotations
	DebugLog(ctx,
		"prepare labels and annotations: labels=%+v annotations=%+v",
		req.GetConfig().GetLabels(),
		req.GetConfig().GetAnnotations(),
	)
	prepareContainerLabels(containerSpec, req.GetConfig(), *image.Id)

	// resolv.conf
	DebugLog(ctx, "prepare resolv.conf: %+v", req.GetSandboxConfig().GetDnsConfig())
	prepareContainerResolvConf(containerSpec, req.GetSandboxConfig().GetDnsConfig())

	// root
	DebugLog(ctx, "prepare container root: %s %s", id, imageName)
	if err = prepareRoot(ctx, containerSpec, volumes, "/"+containerID, imageName); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// mounts
	DebugLog(ctx, "prepare container mounts: %+v", req.GetConfig().GetMounts())
	prepareContainerMounts(ctx, id, volumes, req.GetConfig().GetMounts())

	// devices
	DebugLog(ctx, "prepare container devices: %+v", id)
	prepareContainerDevices(containerSpec, req.GetConfig().GetDevices())

	// spec for UpdateFromSpec for sandbox container
	devicesExplicitValue := true
	portoSandboxId := addPortoPrefix(ctx, &req.PodSandboxId)
	sandboxUpdateSpec := &pb.TContainerSpec{
		Name:            portoSandboxId,
		Devices:         containerSpec.Devices,
		DevicesExplicit: &devicesExplicitValue,
	}

	// update sandbox container
	DebugLog(ctx, "update sandbox container from spec: %+v", id)
	if err = pc.UpdateFromSpec(sandboxUpdateSpec, false); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// create container and roootfs
	DebugLog(ctx, "create container from spec: %+v", id)
	if err = pc.CreateFromSpec(containerSpec, *volumes, false); err != nil {
		DebugLog(ctx, "Failed to CreateFromSpec %v", err)
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// spec reinitialization for UpdateFromSpec
	containerSpec = &pb.TContainerSpec{
		Name: &id,
	}

	// Container prepare order is mandatory!
	// env and command MUST be prepared ONLY AFTER container chroot ready
	DebugLog(ctx, "prepare container environment variables: %+v and %+v", req.GetConfig().GetEnvs(), image.GetConfig().GetEnv())
	prepareEnv(ctx, containerSpec, req.GetConfig().GetEnvs(), image)

	// command + args
	DebugLog(ctx,
		"prepare container command: cmd=%+v args=%+v icmd=%+v",
		req.GetConfig().GetCommand(),
		req.GetConfig().GetArgs(),
		image.GetConfig().GetCmd(),
	)
	if err = prepareCommand(ctx, containerSpec, req.GetConfig().GetCommand(), req.GetConfig().GetArgs(), image.GetConfig().GetCmd(), false); err != nil {
		_ = pc.Destroy(id)
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// set command and environment variables
	DebugLog(ctx, "update container from spec: %+v", id)
	if err = pc.UpdateFromSpec(containerSpec, false); err != nil {
		_ = pc.Destroy(id)
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	return &v1.CreateContainerResponse{
		ContainerId: removePortoPrefix(ctx, id),
	}, nil
}

func (m *PortoshimRuntimeMapper) StartContainer(ctx context.Context, req *v1.StartContainerRequest) (*v1.StartContainerResponse, error) {
	pc := getPortoClient(ctx)

	id := req.GetContainerId()
	parentCnt := getParentCnt(ctx)
	if !isContainer(id, parentCnt) {
		return nil, fmt.Errorf("%s: %s specified ID belongs to pod", getCurrentFuncName(), id)
	}

	DebugLog(ctx, "StartContainer id %+v", id)
	id = *addPortoPrefix(ctx, &id)
	if err := pc.Start(id); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	// Let's do it here not async at first time and think about async later
	if len(Cfg.Porto.ParentContainer) != 0 {
		err := bindMountVolumes(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("Failed to bind mount %v", err)
		}
	}

	return &v1.StartContainerResponse{}, nil
}

func (m *PortoshimRuntimeMapper) StopContainer(ctx context.Context, req *v1.StopContainerRequest) (*v1.StopContainerResponse, error) {
	pc := getPortoClient(ctx)

	id := req.GetContainerId()
	parentCnt := getParentCnt(ctx)
	if !isContainer(id, parentCnt) {
		return nil, fmt.Errorf("%s: %s specified ID belongs to pod", getCurrentFuncName(), id)
	}

	DebugLog(ctx, "StopContainer id %v", id)
	id = *addPortoPrefix(ctx, &id)
	// container existing
	state, err := pc.GetProperty(id, "state")
	if err != nil {
		WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
	}

	if state == "running" {
		if err = pc.Kill(id, 15); err != nil {
			return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
		}
	}

	return &v1.StopContainerResponse{}, nil
}

func (m *PortoshimRuntimeMapper) RemoveContainer(ctx context.Context, req *v1.RemoveContainerRequest) (*v1.RemoveContainerResponse, error) {
	pc := getPortoClient(ctx)

	id := req.GetContainerId()
	parentCnt := getParentCnt(ctx)
	if !isContainer(id, parentCnt) {
		return nil, fmt.Errorf("%s: %s specified ID belongs to pod", getCurrentFuncName(), id)
	}

	DebugLog(ctx, "RemoveContainer id{}", id)

	id = *addPortoPrefix(ctx, &id)
	if err := pc.Destroy(id); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	rootPath := getRootPath(id)
	if err := os.RemoveAll(rootPath); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	return &v1.RemoveContainerResponse{}, nil
}

func (m *PortoshimRuntimeMapper) ListContainers(ctx context.Context, req *v1.ListContainersRequest) (*v1.ListContainersResponse, error) {
	pc := getPortoClient(ctx)
	targetID := req.GetFilter().GetId()
	targetState := req.GetFilter().GetState()
	targetPodSandboxID := req.GetFilter().GetPodSandboxId()
	targetLabels := req.GetFilter().GetLabelSelector()

	mask := ""
	if targetPodSandboxID != "" {
		mask = targetPodSandboxID + "/*"
	}
	if targetID != "" {
		mask = targetID
	}

	response, err := pc.ListContainers(mask)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	var containers []*v1.Container

	parentCnt := getParentCnt(ctx)
	for _, portoid := range response {
		// skip containers with level = 1
		if !isContainer(portoid, parentCnt) {
			DebugLog(ctx, "skip %s as not a container, parent: %v", portoid, parentCnt)
			continue
		}

		props, err := getProperties(ctx, portoid, []string{"labels", "state", "creation_time[raw]"})
		if err != nil {
			WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
			continue
		}
		labels, annotations := convertFromPortoLabels(props["labels"])

		// skip not k8s
		if _, found := labels["portoshim.container.id"]; !found {
			// backward compatibility
			if _, found := labels["io.kubernetes.pod.namespace"]; !found {
				continue
			}
		}

		// filtering
		state := convertContainerState(props["state"])
		if targetState != nil && targetState.GetState() != state {
			continue
		}

		skip := false
		for targetLabel, targetValue := range targetLabels {
			if value, found := labels[targetLabel]; !found || value != targetValue {
				skip = true
				break
			}
		}

		if skip {
			continue
		}

		podID, _ := getPodAndContainer(portoid)
		image := getContainerImage(ctx, portoid, labels)

		containers = append(containers, &v1.Container{
			Id:           removePortoPrefix(ctx, portoid),
			PodSandboxId: podID,
			Metadata:     convertContainerMetadata(labels),
			Image: &v1.ImageSpec{
				Image: image,
			},
			ImageRef:    image,
			State:       state,
			CreatedAt:   convertValueToTime(props["creation_time[raw]"]),
			Labels:      labels,
			Annotations: annotations,
		})
	}

	return &v1.ListContainersResponse{
		Containers: containers,
	}, nil
}

func (m *PortoshimRuntimeMapper) ContainerStatus(ctx context.Context, req *v1.ContainerStatusRequest) (*v1.ContainerStatusResponse, error) {
	id := req.GetContainerId()
	parentCnt := getParentCnt(ctx)
	if !isContainer(id, parentCnt) {
		return nil, fmt.Errorf("%s: specified ID belongs to pod", getCurrentFuncName())
	}

	portoid := *addPortoPrefix(ctx, &id)
	props, err := getProperties(ctx, portoid, []string{"labels", "state", "creation_time[raw]", "start_time[raw]", "death_time[raw]", "exit_code"})
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}
	labels, annotations := convertFromPortoLabels(props["labels"])
	image := getContainerImage(ctx, portoid, labels)

	resp := &v1.ContainerStatusResponse{
		Status: &v1.ContainerStatus{
			Id:         id,
			Metadata:   convertContainerMetadata(labels),
			State:      convertContainerState(props["state"]),
			CreatedAt:  convertValueToTime(props["creation_time[raw]"]),
			StartedAt:  convertValueToTime(props["start_time[raw]"]),
			FinishedAt: convertValueToTime(props["death_time[raw]"]),
			ExitCode:   int32(convertValueToInt(props["exit_code"])),
			Image: &v1.ImageSpec{
				Image: image,
			},
			ImageRef:    image,
			Labels:      labels,
			Annotations: annotations,
			LogPath:     labels["io.kubernetes.container.logpath"],
		},
	}

	return resp, nil
}

func (m *PortoshimRuntimeMapper) UpdateContainerResources(ctx context.Context, req *v1.UpdateContainerResourcesRequest) (*v1.UpdateContainerResourcesResponse, error) {
	return nil, fmt.Errorf("not implemented UpdateContainerResources")
}

func (m *PortoshimRuntimeMapper) ReopenContainerLog(ctx context.Context, req *v1.ReopenContainerLogRequest) (*v1.ReopenContainerLogResponse, error) {
	// TODO: реализовать ReopenContainerLog
	return &v1.ReopenContainerLogResponse{}, nil
}

func (m *PortoshimRuntimeMapper) ExecSync(ctx context.Context, req *v1.ExecSyncRequest) (*v1.ExecSyncResponse, error) {
	pc := getPortoClient(ctx)
	containerID := req.GetContainerId()
	DebugLog(ctx, "ExecSync containerID %v", containerID)
	portoContainerID := *addPortoPrefix(ctx, &containerID)
	id := filepath.Join(portoContainerID, createID("exec-sync"))

	// spec initialization for CreateFromSpec
	containerSpec := &pb.TContainerSpec{
		Name: &id,
		Weak: getBoolPointer(true),
	}

	// environment variables
	env, err := pc.GetProperty(portoContainerID, "env")
	if err != nil {
		return nil, fmt.Errorf("failed to get parent container %s env prop: %w", req.GetContainerId(), err)
	}
	prepareExecEnv(ctx, containerSpec, env)

	// command
	if err := prepareCommand(ctx, containerSpec, req.GetCmd(), nil, nil, true); err != nil {
		return nil, fmt.Errorf("failed to prepare command '%v' for exec container %s: %w", req.Cmd, id, err)
	}

	// create and start container
	if err = pc.CreateFromSpec(containerSpec, []*pb.TVolumeSpec{}, true); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}
	defer pc.Destroy(id)

	timeout := Cfg.Porto.SocketTimeout
	if req.GetTimeout() > 0 {
		timeout = time.Duration(req.GetTimeout()) * time.Second
	}
	if _, err := pc.Wait([]string{id}, timeout); err != nil {
		return nil, fmt.Errorf("failed to wait exec container %s exit: %w", id, err)
	}

	exitCode, err := pc.GetProperty(id, "exit_code")
	if err != nil {
		return nil, fmt.Errorf("failed to get container %s exit_code: %w", id, err)
	}
	code, err := strconv.ParseInt(exitCode, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to parse exit_code '%s': %w", exitCode, err)
	}

	// TODO: maybe read whole stdout/stderr file not just tail from porto?
	streams, err := getProperties(ctx, id, []string{"stdout", "stderr"})
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	rsp := &v1.ExecSyncResponse{
		Stdout:   []byte(streams["stdout"]),
		Stderr:   []byte(streams["stderr"]),
		ExitCode: int32(code),
	}

	return rsp, nil
}

func (m *PortoshimRuntimeMapper) Exec(ctx context.Context, req *v1.ExecRequest) (*v1.ExecResponse, error) {
	resp, err := m.streamingServer.GetExec(req)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare exec endpoint: %v", err)
	}

	return resp, nil
}

func (m *PortoshimRuntimeMapper) Attach(ctx context.Context, req *v1.AttachRequest) (*v1.AttachResponse, error) {
	return nil, fmt.Errorf("not implemented Attach")
}

func (m *PortoshimRuntimeMapper) PortForward(ctx context.Context, req *v1.PortForwardRequest) (*v1.PortForwardResponse, error) {
	return nil, fmt.Errorf("not implemented PortForward")
}

func (m *PortoshimRuntimeMapper) ContainerStats(ctx context.Context, req *v1.ContainerStatsRequest) (*v1.ContainerStatsResponse, error) {
	id := req.GetContainerId()
	parentCnt := getParentCnt(ctx)
	if !isContainer(id, parentCnt) {
		return nil, fmt.Errorf("%s: specified ID belongs to pod", getCurrentFuncName())
	}
	DebugLog(ctx, "ContainerStats id %v", id)
	portoid := *addPortoPrefix(ctx, &id)

	return &v1.ContainerStatsResponse{
		Stats: getContainerStats(ctx, id, portoid),
	}, nil
}

func (m *PortoshimRuntimeMapper) ListContainerStats(ctx context.Context, req *v1.ListContainerStatsRequest) (*v1.ListContainerStatsResponse, error) {
	pc := getPortoClient(ctx)

	targetID := req.GetFilter().GetId()
	targetPodSandboxID := req.GetFilter().GetPodSandboxId()
	targetLabels := req.GetFilter().GetLabelSelector()

	mask := ""
	if targetPodSandboxID != "" {
		mask = targetPodSandboxID + "/*"
	}
	if targetID != "" {
		mask = targetID
	}

	mask = *addPortoPrefix(ctx, &mask)

	response, err := pc.ListContainers(mask)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	var stats []*v1.ContainerStats

	parentCnt := getParentCnt(ctx)
	for _, portoid := range response {
		// skip containers with level = 1
		if !isContainer(portoid, parentCnt) {
			continue
		}

		props, err := getProperties(ctx, portoid, []string{"labels", "state"})
		if err != nil {
			WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
			continue
		}
		labels, _ := convertFromPortoLabels(props["labels"])

		// skip not k8s
		if _, found := labels["portoshim.container.id"]; !found {
			// backward compatibility
			if _, found := labels["io.kubernetes.pod.namespace"]; !found {
				continue
			}
		}

		skip := false
		for targetLabel, targetValue := range targetLabels {
			if value, found := labels[targetLabel]; !found || value != targetValue {
				skip = true
				break
			}
		}

		if skip {
			continue
		}

		id := removePortoPrefix(ctx, portoid)
		stats = append(stats, getContainerStats(ctx, id, portoid))
	}

	return &v1.ListContainerStatsResponse{
		Stats: stats,
	}, nil
}

func (m *PortoshimRuntimeMapper) ListPodSandboxStats(ctx context.Context, req *v1.ListPodSandboxStatsRequest) (*v1.ListPodSandboxStatsResponse, error) {
	pc := getPortoClient(ctx)

	targetID := req.GetFilter().GetId()
	targetLabels := req.GetFilter().GetLabelSelector()

	mask := "*"
	if targetID != "" {
		mask = targetID
	}

	mask = *addPortoPrefix(ctx, &mask)
	response, err := pc.ListContainers(mask)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	var stats []*v1.PodSandboxStats
	for _, portoid := range response {
		props, err := getProperties(ctx, portoid, []string{"labels", "state"})
		if err != nil {
			WarnLog(ctx, "%s: %v", getCurrentFuncName(), err)
			continue
		}
		labels, _ := convertFromPortoLabels(props["labels"])

		// skip not k8s
		if _, found := labels["portoshim.pod.id"]; !found {
			// backward compatibility
			if _, found := labels["io.kubernetes.pod.namespace"]; !found {
				continue
			}
		}

		skip := false
		for targetLabel, targetValue := range targetLabels {
			if value, found := labels[targetLabel]; !found || value != targetValue {
				skip = true
				break
			}
		}

		if skip {
			continue
		}

		id := removePortoPrefix(ctx, portoid)
		stats = append(stats, getPodStats(ctx, id, portoid))
	}

	return &v1.ListPodSandboxStatsResponse{
		Stats: stats,
	}, nil
}

func (m *PortoshimRuntimeMapper) UpdateRuntimeConfig(ctx context.Context, req *v1.UpdateRuntimeConfigRequest) (*v1.UpdateRuntimeConfigResponse, error) {
	return nil, fmt.Errorf("not implemented UpdateRuntimeConfig")
}

func (m *PortoshimRuntimeMapper) Status(ctx context.Context, req *v1.StatusRequest) (*v1.StatusResponse, error) {
	pc := getPortoClient(ctx)

	if _, _, err := pc.GetVersion(); err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	var conditions []*v1.RuntimeCondition
	conditions = append(conditions, &v1.RuntimeCondition{
		Type:   "RuntimeReady",
		Status: true,
	})
	conditions = append(conditions, &v1.RuntimeCondition{
		Type:   "NetworkReady",
		Status: true,
	})

	return &v1.StatusResponse{
		Status: &v1.RuntimeStatus{
			Conditions: conditions,
		},
	}, nil
}
