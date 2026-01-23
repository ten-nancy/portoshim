package main

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/ten-nancy/porto/src/api/go/porto"
	pb "github.com/ten-nancy/porto/src/api/go/porto/pkg/rpc"
	v1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type PortoshimImageMapper struct{}

// INTERNAL
func parseImageName(name string) (string, string, string) {
	image, digest, _ := strings.Cut(name, "@")
	image, tag, tagFound := strings.Cut(image, ":")
	if !tagFound {
		tag = "latest"
	}

	return image, tag, digest
}
func getImageStruct(image *pb.TDockerImage) *v1.Image {
	return &v1.Image{
		Id:          image.GetId(),
		RepoTags:    image.GetTags(),
		RepoDigests: image.GetDigests(),
		Size_:       image.GetSize(),
		Uid: &v1.Int64Value{
			Value: 1,
		},
		Username: "",
		Spec:     nil,
	}
}

// IMAGE SERVICE INTERFACE
func (m *PortoshimImageMapper) ListImages(ctx context.Context, req *v1.ListImagesRequest) (*v1.ListImagesResponse, error) {
	pc := getPortoClient(ctx)

	portoImages, err := pc.ListDockerImages("", "")
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	var images []*v1.Image
	for _, image := range portoImages {
		images = append(images, getImageStruct(image))
	}

	return &v1.ListImagesResponse{
		Images: images,
	}, nil
}
func (m *PortoshimImageMapper) ImageStatus(ctx context.Context, req *v1.ImageStatusRequest) (*v1.ImageStatusResponse, error) {
	pc := getPortoClient(ctx)

	image, err := pc.DockerImageStatus(req.GetImage().GetImage(), Cfg.Images.Place)
	if err != nil {
		if err.(*porto.PortoError).Code == pb.EError_DockerImageNotFound {
			return &v1.ImageStatusResponse{
				Image: nil,
				Info:  map[string]string{},
			}, nil
		}
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	return &v1.ImageStatusResponse{
		Image: getImageStruct(image),
	}, nil
}
func (m *PortoshimImageMapper) PullImage(ctx context.Context, req *v1.PullImageRequest) (*v1.PullImageResponse, error) {
	pc := getPortoClient(ctx)

	DebugLog(ctx, "pull image: %s, %s", Cfg.Images.PauseImage, req.GetImage().GetImage())
	registry := GetImageRegistry(req.GetImage().GetImage())
	authToken := registry.AuthToken
	if authToken == "" && req.GetAuth() != nil && req.GetAuth().GetPassword() != "" {
		authToken = req.GetAuth().GetPassword()
	}

	dockerImage := porto.DockerImage{
		Name:  req.GetImage().GetImage(),
		Place: Cfg.Images.Place,
	}
	registryCreds := porto.DockerRegistryCredentials{
		AuthToken:   authToken,
		AuthPath:    registry.AuthPath,
		AuthService: registry.AuthService,
	}
	image, err := pc.PullDockerImage(dockerImage, registryCreds)
	if err != nil {
		return nil, fmt.Errorf("Failed to pull docker image %s: %v", getCurrentFuncName(), err)
	}

	return &v1.PullImageResponse{
		ImageRef: image.GetId(),
	}, nil
}
func (m *PortoshimImageMapper) RemoveImage(ctx context.Context, req *v1.RemoveImageRequest) (*v1.RemoveImageResponse, error) {
	pc := getPortoClient(ctx)

	err := pc.RemoveDockerImage(req.GetImage().GetImage(), Cfg.Images.Place)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	return &v1.RemoveImageResponse{}, nil
}
func (m *PortoshimImageMapper) ImageFsInfo(ctx context.Context, req *v1.ImageFsInfoRequest) (*v1.ImageFsInfoResponse, error) {
	stat := syscall.Statfs_t{}
	err := syscall.Statfs(Cfg.Porto.ImagesDir, &stat)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	return &v1.ImageFsInfoResponse{
		ImageFilesystems: []*v1.FilesystemUsage{
			{
				Timestamp: time.Now().UnixNano(),
				FsId: &v1.FilesystemIdentifier{
					Mountpoint: Cfg.Porto.ImagesDir,
				},
				UsedBytes:  &v1.UInt64Value{Value: (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)},
				InodesUsed: &v1.UInt64Value{Value: stat.Files - stat.Ffree},
			},
		},
	}, nil
}
