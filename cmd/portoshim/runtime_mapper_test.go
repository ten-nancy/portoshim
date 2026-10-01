package main

import (
	"context"
	"fmt"
	"math/rand"
	"path"
	"strings"
	"testing"

	cni "github.com/containerd/go-cni"
	"github.com/ten-nancy/porto/src/api/go/porto"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const portoName = "ISS-AGENT--alexperevalov-portoshim-stage-48/box/ps-87378-0"

// for portoAPI testing purpose
func setCFG(t *testing.T) {
	err := InitConfig("")

	if err != nil {
		t.Fatalf("Failed to init config %v", err)
	}
	Cfg.Porto.ParentContainer = portoName
}

func TestIsPod(t *testing.T) {
	setCFG(t)

	for ntest, test := range []struct {
		name     string
		podname  string
		expected bool
	}{
		{
			name:     "test1",
			podname:  path.Join(portoName, "pod1"),
			expected: true,
		}, {
			name:     "test2",
			podname:  path.Join(portoName, "pod1/cnt1"),
			expected: false,
		},
	} {
		t.Logf("Test #%d", ntest)
		if isPod(test.podname, portoName) != test.expected {
			t.Fatalf("test: %v pod name: %v expected %v, PORTO_NAME: %v", test.name, test.podname, test.expected, portoName)
		}
	}
}

func TestIsContainer(t *testing.T) {
	setCFG(t)

	for ntest, test := range []struct {
		name     string
		cntname  string
		expected bool
	}{
		{
			name:     "test1",
			cntname:  path.Join(portoName, "cnt1"),
			expected: false,
		}, {
			name:     "test2",
			cntname:  path.Join(portoName, "pod1/cnt1"),
			expected: true,
		}, {
			name:     "test3",
			cntname:  path.Join("pod1", "cnt1", privCntName),
			expected: false,
		},
	} {
		t.Logf("Test #%d", ntest)
		if isContainer(test.cntname, portoName) != test.expected {
			t.Fatalf("test: %v cnt name: %v expected %v, PORTO_NAME: %v", test.name, test.cntname, test.expected, portoName)
		}
	}
}

func TestPrepareContainerMounts(t *testing.T) {

	mounts := []*v1.Mount{
		&v1.Mount{
			ContainerPath: "/",
			HostPath:      "/cnt/",
		},
		&v1.Mount{
			ContainerPath: "/test/file1",
			HostPath:      "/cnt/file1",
		},
		&v1.Mount{
			ContainerPath: "/test/",
			HostPath:      "/cnt/test/",
		},
	}

	resultVolumes := &[]*pb.TVolumeSpec{
		&pb.TVolumeSpec{
			Links: []*pb.TVolumeLink{&pb.TVolumeLink{
				Container: getStringPointer("cnt1"),
				Target:    getStringPointer("/"),
			}},
		},
		&pb.TVolumeSpec{
			Links: []*pb.TVolumeLink{&pb.TVolumeLink{
				Container: getStringPointer("cnt1"),
				Target:    getStringPointer("/test"),
			}},
		},
		&pb.TVolumeSpec{
			Links: []*pb.TVolumeLink{&pb.TVolumeLink{
				Container: getStringPointer("cnt1"),
				Target:    getStringPointer("/test/file1"),
			}},
		},
		&pb.TVolumeSpec{
			Links: []*pb.TVolumeLink{&pb.TVolumeLink{
				Container: getStringPointer("cnt1"),
				Target:    getStringPointer("/usr/sbin/logshim"),
			}},
		},
	}

	ctx := context.Background()

	waitPathes := make(map[string][]string, 0)
	for ntest, test := range []struct {
		name            string
		mounts          []*v1.Mount
		expectedVolumes *[]*pb.TVolumeSpec
	}{
		{
			name:            "one",
			mounts:          mounts,
			expectedVolumes: resultVolumes,
		},
	} {
		t.Logf("Test #%d", ntest)
		volumes := &[]*pb.TVolumeSpec{}
		prepareContainerMounts(ctx, "cnt1", volumes, test.mounts, waitPathes)
		// prepareContainerMounts adds additional path for logshim
		if len(*volumes) != len(*test.expectedVolumes) {
			t.Fatalf("result Volumes count %d %d", len(*volumes), len(*test.expectedVolumes))
		}
		for i, volume := range *(test.expectedVolumes) {
			if *(*volumes)[i].Links[0].Target != *volume.Links[0].Target {
				t.Fatalf("order %d gotten %v expected %v", i, *(*volumes)[i].Links[0].Target, *volume.Links[0].Target)
			}
		}
	}
}

// RuntimeMapper

func NewFakePortoshimRuntimeMapper() (*PortoshimRuntimeMapper, error) {
	rm := &PortoshimRuntimeMapper{}
	fakeNetPlugin, err := cni.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cni: %v", err)
	}
	rm.netPlugin = fakeNetPlugin
	return rm, nil
}

func TestRunPodSandbox(t *testing.T) {
	rm, err := NewFakePortoshimRuntimeMapper()
	if err != nil {
		t.Fatalf("NewFakePortoshimRuntimeMapper faled")
	}
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx := context.Background()
	initFakeConfig(t)
	fakePortoClient := porto.NewMockPortoAPI(ctrl)

	fakePortoClient.EXPECT().Connect().Return(nil)
	fakePortoClient.EXPECT().GetProperty(gomock.Any(), gomock.Any()).DoAndReturn(func(id, property string) (string, error) {
		if strings.HasSuffix(id, "/"+privCntName) {
			return "", fmt.Errorf("container does not exist")
		}
		return "", nil
	}).AnyTimes()
	fakePortoClient.EXPECT().CreateFromSpec(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	fakePortoClient.EXPECT().Destroy(gomock.Any()).Return(nil).AnyTimes()
	fakePortoClient.EXPECT().UpdateFromSpec(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	err = fakePortoClient.Connect()
	if err != nil {
		t.Fatalf("Can't connect")
	}
	//nolint:sa1029
	ctx = context.WithValue(ctx, "portoClient", fakePortoClient)
	//nolint:sa1029
	ctx = context.WithValue(ctx, "requestId", fmt.Sprintf("%08x", rand.Intn(4294967296)))

	req := v1.RunPodSandboxRequest{
		Config: &v1.PodSandboxConfig{
			Metadata: &v1.PodSandboxMetadata{
				Name: t.Name(),
			},
		},
	}

	// TEST
	type RunPodSandboxTest struct {
		image      string
		retImage   *pb.TDockerImage
		retError   error
		expectFunc func(test *RunPodSandboxTest, m *porto.MockPortoAPI)
	}

	for _, test := range []RunPodSandboxTest{
		// Test dont pull, since no error in DockerImageStatus
		{
			image: Cfg.Images.PauseImage,
			retImage: &pb.TDockerImage{
				Config: &pb.TDockerImageConfig{
					Cmd: []string{"sleep", "inf"},
				},
			},
			retError: nil,
		},
		// Test pull docker image, since DockerImageStatus returns error
		{
			image: Cfg.Images.PauseImage,
			retImage: &pb.TDockerImage{
				Config: &pb.TDockerImageConfig{
					Cmd: []string{"sleep", "inf"},
				},
			},
			retError: fmt.Errorf("ERROR"),
			expectFunc: func(ct *RunPodSandboxTest, m *porto.MockPortoAPI) {
				m.EXPECT().PullDockerImage(gomock.Any(), gomock.Any()).Return(
					ct.retImage, nil)
			},
		},
	} {
		if test.expectFunc != nil {
			test.expectFunc(&test, fakePortoClient)
		}
		fakePortoClient.EXPECT().DockerImageStatus(test.image, Cfg.Images.Place).DoAndReturn(
			func(name, place string) (*pb.TDockerImage, error) {
				return test.retImage, test.retError
			})

		_, err = rm.RunPodSandbox(ctx, &req)
		if err != nil {
			t.Fatalf("Failed to RunPodSandbox: %v", err)
		}
	}
}

func TestCreateContainerAndListContainers(t *testing.T) {
	rm, err := NewFakePortoshimRuntimeMapper()
	if err != nil {
		t.Fatalf("NewFakePortoshimRuntimeMapper failed")
	}
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx := context.Background()
	initFakeConfig(t)
	// yaml.Unmarshal in InitConfig merges into existing Cfg, so a ParentContainer
	// set by an earlier test (e.g. setCFG in TestIsContainer) can leak in. Reset
	// it explicitly so prepareContainerLogs takes the no-mkdir branch.
	Cfg.Porto.ParentContainer = ""
	fakePortoClient := porto.NewMockPortoAPI(ctrl)

	fakePortoClient.EXPECT().Connect().Return(nil)
	fakePortoClient.EXPECT().GetProperty(gomock.Any(), gomock.Any()).DoAndReturn(func(id, property string) (string, error) {
		if strings.HasSuffix(id, "/"+privCntName) {
			return "", fmt.Errorf("container does not exist")
		}
		return "", nil
	}).AnyTimes()
	fakePortoClient.EXPECT().CreateFromSpec(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	fakePortoClient.EXPECT().UpdateFromSpec(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	fakePortoClient.EXPECT().Destroy(gomock.Any()).Return(nil).AnyTimes()
	// createAndConfigurePrivCnt issues an extra Start for the privileged helper.
	fakePortoClient.EXPECT().Start(gomock.Any()).Return(nil).AnyTimes()

	if err = fakePortoClient.Connect(); err != nil {
		t.Fatalf("Can't connect")
	}
	//nolint:sa1029
	ctx = context.WithValue(ctx, "portoClient", fakePortoClient)
	//nolint:sa1029
	ctx = context.WithValue(ctx, "requestId", fmt.Sprintf("%08x", rand.Intn(4294967296)))

	const imageName = "test.registry/test/image:latest"
	imageID := "test-image-id"

	type CreateAndListTest struct {
		name          string
		podID         string
		containerName string
		privileged    bool
	}

	for ntest, test := range []CreateAndListTest{
		{
			name:          "non-privileged container",
			podID:         "testpod",
			containerName: "testcnt",
			privileged:    false,
		},
		{
			name:          "privileged container",
			podID:         "privpod",
			containerName: "privcnt",
			privileged:    true,
		},
	} {
		t.Logf("Test #%d: %s", ntest, test.name)

		fakePortoClient.EXPECT().DockerImageStatus(imageName, Cfg.Images.Place).Return(
			&pb.TDockerImage{
				Id: &imageID,
				Config: &pb.TDockerImageConfig{
					Cmd: []string{"sleep", "inf"},
				},
			}, nil)

		sandboxCfg := &v1.PodSandboxConfig{
			Metadata: &v1.PodSandboxMetadata{
				Name: test.podID,
			},
		}
		if test.privileged {
			sandboxCfg.Linux = &v1.LinuxPodSandboxConfig{
				SecurityContext: &v1.LinuxSandboxSecurityContext{
					Privileged: true,
				},
			}
		}

		createReq := &v1.CreateContainerRequest{
			PodSandboxId: test.podID,
			Config: &v1.ContainerConfig{
				Metadata: &v1.ContainerMetadata{
					Name: test.containerName,
				},
				Image: &v1.ImageSpec{
					Image: imageName,
				},
			},
			SandboxConfig: sandboxCfg,
		}

		createReq.Config.Linux = &v1.LinuxContainerConfig{
			SecurityContext: &v1.LinuxContainerSecurityContext{Privileged: test.privileged},
		}

		createResp, err := rm.CreateContainer(ctx, createReq)
		if err != nil {
			t.Fatalf("[%s] Failed to CreateContainer: %v", test.name, err)
		}
		createdID := createResp.GetContainerId()
		if createdID == "" {
			t.Fatalf("[%s] CreateContainer returned empty container ID", test.name)
		}

		// Porto label string that survives convertFromPortoLabels and marks the
		// container as k8s (portoshim.container.id present).
		portoLabels := fmt.Sprintf("%s:%s;%s:%s",
			encodeLabel("portoshim.container.id", "LABEL"),
			encodeLabel(createdID, ""),
			encodeLabel("portoshim.container.image", "LABEL"),
			encodeLabel(imageName, ""),
		)
		ids := []string{createdID}
		properties := map[string]map[string]string{
			createdID: {"labels": portoLabels, "state": "running", "creation_time[raw]": "1700000000"},
		}
		if test.privileged {
			properties[createdID]["labels"] += ";" + encodeLabel("priv", "LABEL") + ":" + encodeLabel("true", "")
			child := createdID + "/" + privCntName
			ids = append(ids, child)
			properties[child] = map[string]string{"state": "running", "creation_time[raw]": "1700000000"}
		}

		fakePortoClient.EXPECT().ListContainers("").Return(ids, nil)
		fakePortoClient.EXPECT().GetProperties(ids, []string{"labels", "state", "creation_time[raw]"}).Return(properties, nil)

		listResp, err := rm.ListContainers(ctx, &v1.ListContainersRequest{})
		if err != nil {
			t.Fatalf("[%s] Failed to ListContainers: %v", test.name, err)
		}

		if len(listResp.GetContainers()) != 1 {
			t.Fatalf("[%s] Expected 1 container in ListContainers, got %d",
				test.name, len(listResp.GetContainers()))
		}

		got := listResp.GetContainers()[0]
		if got.GetId() != createdID {
			t.Fatalf("[%s] Container ID mismatch: got %q, want %q", test.name, got.GetId(), createdID)
		}
		if got.GetImage().GetImage() != imageName {
			t.Fatalf("[%s] Container image mismatch: got %q, want %q", test.name, got.GetImage().GetImage(), imageName)
		}
		if got.GetState() != v1.ContainerState_CONTAINER_RUNNING {
			t.Fatalf("[%s] Container state mismatch: got %v, want CONTAINER_RUNNING", test.name, got.GetState())
		}
	}
}

func TestContainerStatus(t *testing.T) {
	rm, err := NewFakePortoshimRuntimeMapper()
	if err != nil {
		t.Fatalf("NewFakePortoshimRuntimeMapper failed")
	}
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx := context.Background()
	initFakeConfig(t)
	// See TestCreateContainerAndListContainers: InitConfig merges into the existing
	// Cfg, so reset ParentContainer to keep prepareContainerLogs on the no-mkdir
	// branch and to keep porto IDs equal to CRI IDs.
	Cfg.Porto.ParentContainer = ""
	fakePortoClient := porto.NewMockPortoAPI(ctrl)

	fakePortoClient.EXPECT().Connect().Return(nil)
	fakePortoClient.EXPECT().GetProperty(gomock.Any(), gomock.Any()).DoAndReturn(func(id, property string) (string, error) {
		if strings.HasSuffix(id, "/"+privCntName) {
			return "", fmt.Errorf("container does not exist")
		}
		return "", nil
	}).AnyTimes()
	fakePortoClient.EXPECT().CreateFromSpec(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	fakePortoClient.EXPECT().UpdateFromSpec(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	if err = fakePortoClient.Connect(); err != nil {
		t.Fatalf("Can't connect")
	}
	//nolint:sa1029
	ctx = context.WithValue(ctx, "portoClient", fakePortoClient)
	//nolint:sa1029
	ctx = context.WithValue(ctx, "requestId", fmt.Sprintf("%08x", rand.Intn(4294967296)))

	const imageName = "test.registry/test/image:latest"
	imageID := "test-image-id"

	type CreateAndCheckStatusTest struct {
		name          string
		podID         string
		containerName string
		state         string
		creationTime  string
		startTime     string
		deathTime     string
		exitCode      int
		expectedState v1.ContainerState
	}

	for ntest, test := range []CreateAndCheckStatusTest{
		{
			name:          "running container",
			podID:         "testpod1",
			containerName: "testcnt1",
			state:         "running",
			creationTime:  "1700000000",
			startTime:     "1700000005",
			deathTime:     "0",
			exitCode:      0,
			expectedState: v1.ContainerState_CONTAINER_RUNNING,
		},
		{
			name:          "dead container",
			podID:         "testpod2",
			containerName: "testcnt2",
			state:         "dead",
			creationTime:  "1700000100",
			startTime:     "1700000105",
			deathTime:     "1700000200",
			exitCode:      137,
			expectedState: v1.ContainerState_CONTAINER_EXITED,
		},
	} {
		t.Logf("Test #%d: %s", ntest, test.name)

		fakePortoClient.EXPECT().DockerImageStatus(imageName, Cfg.Images.Place).Return(
			&pb.TDockerImage{
				Id: &imageID,
				Config: &pb.TDockerImageConfig{
					Cmd: []string{"sleep", "inf"},
				},
			}, nil)

		sandboxCfg := &v1.PodSandboxConfig{
			Metadata: &v1.PodSandboxMetadata{
				Name: test.podID,
			},
		}

		createReq := &v1.CreateContainerRequest{
			PodSandboxId: test.podID,
			Config: &v1.ContainerConfig{
				Metadata: &v1.ContainerMetadata{
					Name: test.containerName,
				},
				Image: &v1.ImageSpec{
					Image: imageName,
				},
			},
			SandboxConfig: sandboxCfg,
		}

		createResp, err := rm.CreateContainer(ctx, createReq)
		if err != nil {
			t.Fatalf("[%s] Failed to CreateContainer: %v", test.name, err)
		}
		createdID := createResp.GetContainerId()
		if createdID == "" {
			t.Fatalf("[%s] CreateContainer returned empty container ID", test.name)
		}

		// Labels as prepareContainerLabels would have written them, encoded the way
		// porto reports them back so convertFromPortoLabels can decode them.
		expectedLogPath := path.Join("/place/porto", createdID, "stdout")
		pairs := []string{}
		for label, value := range convertToPortoLabels(map[string]string{
			"attempt":                         "0",
			"io.kubernetes.container.name":    test.containerName,
			"io.kubernetes.container.logpath": expectedLogPath,
			"portoshim.container.id":          createdID,
			"portoshim.container.image":       imageName,
		}, nil) {
			pairs = append(pairs, label+":"+value)
		}
		portoLabels := strings.Join(pairs, ";")

		props := map[string]string{
			"labels":             portoLabels,
			"state":              test.state,
			"creation_time[raw]": test.creationTime,
			"start_time[raw]":    test.startTime,
			"death_time[raw]":    test.deathTime,
			"exit_code":          fmt.Sprint(test.exitCode),
		}
		keyval := []*pb.TGetResponse_TContainerGetValueResponse{}
		for variable, value := range props {
			keyval = append(keyval, &pb.TGetResponse_TContainerGetValueResponse{
				Variable: getStringPointer(variable),
				Value:    getStringPointer(value),
			})
		}
		getResp := &pb.TGetResponse{
			List: []*pb.TGetResponse_TContainerGetListResponse{
				{
					Name:   getStringPointer(createdID),
					Keyval: keyval,
				},
			},
		}

		fakePortoClient.EXPECT().Get([]string{createdID}, gomock.Any()).Return(getResp, nil)

		// A dead container is reaped right after its status is reported.
		if test.state == "dead" {
			fakePortoClient.EXPECT().Destroy(createdID).Return(nil)
		}

		statusRsp, err := rm.ContainerStatus(ctx, &v1.ContainerStatusRequest{
			ContainerId: createdID,
		})
		if err != nil {
			t.Fatalf("[%s] Failed to ContainerStatus: %v", test.name, err)
		}

		got := statusRsp.GetStatus()
		if got == nil {
			t.Fatalf("[%s] ContainerStatus returned no status", test.name)
		}
		if got.GetId() != createdID {
			t.Fatalf("[%s] Container ID mismatch: got %q, want %q", test.name, got.GetId(), createdID)
		}
		if got.GetMetadata().GetName() != test.containerName {
			t.Fatalf("[%s] Container name mismatch: got %q, want %q",
				test.name, got.GetMetadata().GetName(), test.containerName)
		}
		if got.GetState() != test.expectedState {
			t.Fatalf("[%s] Container state mismatch: got %v, want %v",
				test.name, got.GetState(), test.expectedState)
		}
		if got.GetCreatedAt() != convertValueToTime(test.creationTime) {
			t.Fatalf("[%s] CreatedAt mismatch: got %d, want %d",
				test.name, got.GetCreatedAt(), convertValueToTime(test.creationTime))
		}
		if got.GetStartedAt() != convertValueToTime(test.startTime) {
			t.Fatalf("[%s] StartedAt mismatch: got %d, want %d",
				test.name, got.GetStartedAt(), convertValueToTime(test.startTime))
		}
		if got.GetFinishedAt() != convertValueToTime(test.deathTime) {
			t.Fatalf("[%s] FinishedAt mismatch: got %d, want %d",
				test.name, got.GetFinishedAt(), convertValueToTime(test.deathTime))
		}
		if got.GetExitCode() != int32(test.exitCode) {
			t.Fatalf("[%s] ExitCode mismatch: got %d, want %d", test.name, got.GetExitCode(), test.exitCode)
		}
		if got.GetImage().GetImage() != imageName || got.GetImageRef() != imageName {
			t.Fatalf("[%s] Container image mismatch: got %q/%q, want %q",
				test.name, got.GetImage().GetImage(), got.GetImageRef(), imageName)
		}
		if got.GetLogPath() != expectedLogPath {
			t.Fatalf("[%s] LogPath mismatch: got %q, want %q", test.name, got.GetLogPath(), expectedLogPath)
		}
	}

	// A pod ID (no container part) must be rejected without touching porto.
	if _, err := rm.ContainerStatus(ctx, &v1.ContainerStatusRequest{ContainerId: "testpod1"}); err == nil {
		t.Fatalf("ContainerStatus accepted a pod ID, expected an error")
	}
}

func TestPrivilegedContainerLifecycle(t *testing.T) {
	for _, state := range []string{"running", "dead"} {
		t.Run(state, func(t *testing.T) {
			initFakeConfig(t)
			Cfg.Porto.ParentContainer = ""
			ctrl := gomock.NewController(t)
			pc := porto.NewMockPortoAPI(ctrl)
			//nolint:sa1029
			ctx := context.WithValue(context.Background(), "portoClient", pc)
			//nolint:sa1029
			ctx = context.WithValue(ctx, "requestId", "test")
			const id = "pod/init"
			const child = id + "/privileged"
			pairs := []string{}
			for k, v := range convertToPortoLabels(map[string]string{
				"priv": "true", "portoshim.container.id": id,
				"portoshim.container.image": "image", "io.kubernetes.container.name": "init",
				"io.kubernetes.container.logpath": "/logs/init", "attempt": "0",
			}, nil) {
				pairs = append(pairs, k+":"+v)
			}
			parentProps := map[string]string{"labels": strings.Join(pairs, ";"), "state": "running", "creation_time[raw]": "10"}
			childProps := map[string]string{"state": state, "creation_time[raw]": "11", "start_time[raw]": "12", "death_time[raw]": "22", "exit_code": "0"}
			pc.EXPECT().GetProperty(child, "state").Return(state, nil).Times(1)
			pc.EXPECT().GetProperty("", "absolute_name").Return("", nil).AnyTimes()
			pc.EXPECT().GetProperties([]string{"pod", id, child}, []string{"labels", "state", "creation_time[raw]"}).Return(map[string]map[string]string{
				"pod": {},
				id:    parentProps,
				child: childProps,
			}, nil).Times(2)
			pc.EXPECT().GetProperties([]string{id, child}, []string{"labels", "state", "creation_time[raw]"}).Return(map[string]map[string]string{
				id:    parentProps,
				child: childProps,
			}, nil).Times(1)
			pc.EXPECT().Get(gomock.Any(), gomock.Any()).DoAndReturn(func(ids, names []string) (*pb.TGetResponse, error) {
				result := &pb.TGetResponse{}
				for _, containerID := range ids {
					props := parentProps
					if containerID == child {
						props = childProps
					} else if containerID != id {
						t.Fatalf("unexpected container %s", containerID)
					}
					values := []*pb.TGetResponse_TContainerGetValueResponse{}
					for _, key := range names {
						values = append(values, &pb.TGetResponse_TContainerGetValueResponse{Variable: getStringPointer(key), Value: getStringPointer(props[key])})
					}
					result.List = append(result.List, &pb.TGetResponse_TContainerGetListResponse{Name: getStringPointer(containerID), Keyval: values})
				}
				return result, nil
			}).Times(2) // Child status and parent metadata.

			pc.EXPECT().ListContainers("").Return([]string{"pod", id, child}, nil).Times(2)
			pc.EXPECT().ListContainers("pod/***").Return([]string{id, child, "pod/other", "pod/other/privileged"}, nil).Times(1)
			mapper := &PortoshimRuntimeMapper{}
			want := v1.ContainerState_CONTAINER_RUNNING
			if state == "dead" {
				want = v1.ContainerState_CONTAINER_EXITED
				pc.EXPECT().Destroy(id).Return(nil)
			}
			listed, err := mapper.ListContainers(ctx, &v1.ListContainersRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(listed.Containers) != 1 || listed.Containers[0].Id != id || listed.Containers[0].State != want {
				t.Fatalf("incorrect listing: %v", listed)
			}
			filtered, err := mapper.ListContainers(ctx, &v1.ListContainersRequest{Filter: &v1.ContainerFilter{State: &v1.ContainerStateValue{State: v1.ContainerState_CONTAINER_EXITED}}})
			if err != nil {
				t.Fatal(err)
			}
			if (len(filtered.Containers) == 1) != (state == "dead") {
				t.Fatalf("incorrect state filter: %v", filtered)
			}
			byID, err := mapper.ListContainers(ctx, &v1.ListContainersRequest{Filter: &v1.ContainerFilter{Id: id}})
			if err != nil {
				t.Fatal(err)
			}
			if len(byID.Containers) != 1 || byID.Containers[0].State != want {
				t.Errorf("incorrect ID filter: %v", byID)
			}
			status, err := mapper.ContainerStatus(ctx, &v1.ContainerStatusRequest{ContainerId: id})
			if err != nil {
				t.Fatal(err)
			}
			if status.Status.Id != id || status.Status.State != want || status.Status.Metadata.Name != "init" || status.Status.LogPath != "/logs/init" || status.Status.StartedAt != convertValueToTime("12") {
				t.Fatalf("incorrect status: %v", status)
			}
		})
	}
}
