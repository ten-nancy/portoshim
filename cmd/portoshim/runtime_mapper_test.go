package main

import (
	"context"
	"fmt"
	"math/rand"
	"path"
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
	fakePortoClient.EXPECT().GetProperty(gomock.Any(), gomock.Any()).Return("", nil).AnyTimes()
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
	fakePortoClient.EXPECT().GetProperty(gomock.Any(), gomock.Any()).Return("", nil).AnyTimes()
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
		labelsVar := "labels"
		stateVar := "state"
		creationTimeVar := "creation_time[raw]"
		stateVal := "running"
		creationTimeVal := "1700000000"
		respName := createdID

		getResp := &pb.TGetResponse{
			List: []*pb.TGetResponse_TContainerGetListResponse{
				{
					Name: &respName,
					Keyval: []*pb.TGetResponse_TContainerGetValueResponse{
						{Variable: &labelsVar, Value: &portoLabels},
						{Variable: &stateVar, Value: &stateVal},
						{Variable: &creationTimeVar, Value: &creationTimeVal},
					},
				},
			},
		}

		fakePortoClient.EXPECT().ListContainers("").Return([]string{createdID}, nil)
		fakePortoClient.EXPECT().Get([]string{createdID}, gomock.Any()).Return(getResp, nil)

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
