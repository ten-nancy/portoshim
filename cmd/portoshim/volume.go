package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	bindMountSuffixes = []string{
		"/var/run/secrets/kubernetes.io/serviceaccount",
	}
)

func bindMount(source, dest string) error {
	return unix.Mount(source, dest, "", uintptr(unix.MS_BIND|unix.MS_REC), "")
}

func bindMountVolumes(ctx context.Context, id string) error {
	props, err := getProperties(ctx, id, []string{"volumes_linked"})
	if err != nil {
		return fmt.Errorf("%s: %v", getCurrentFuncName(), err)
	}

	linkedVols, ok := props["volumes_linked"]
	if !ok {
		return fmt.Errorf("no property volumes_linked")
	}
	pc := getPortoClient(ctx)
	DebugLog(ctx, "volumes_linked: %v", linkedVols)
	vols := strings.Split(linkedVols, ";")
	for _, volume := range vols {
		links := strings.Split(strings.TrimSpace(volume), " ")
		if len(links) < 2 {
			DebugLog(ctx, "len(links) %d for %s", len(links), volume)
			continue
		}
		// TODO for other types of volumes, we need here something general
		for _, bindMountSuffix := range bindMountSuffixes {
			if !strings.HasSuffix(links[0], bindMountSuffix) {
				DebugLog(ctx, "Suffix %s not found in %s", bindMountSuffix, links[0])
				continue
			}
			volumeDesc, err := pc.GetVolume(links[0])
			if err != nil {
				WarnLog(ctx, "Failed to get volume %v", err)
				continue
			}
			volumeDescJson, _ := json.Marshal(volumeDesc)
			DebugLog(ctx, "volume: %v", string(volumeDescJson))

			storagePath, ok := volumeDesc.Properties["storage"]
			if !ok {
				volumeDescJson, _ := json.Marshal(volumeDesc)
				WarnLog(ctx, "Can't find storage in volume description %v", volumeDescJson)
				continue
			}
			// mount --bind storage links[0]
			_, err = os.Stat(storagePath)
			if err != nil {
				WarnLog(ctx, "%s doesn't exist or not accessible", storagePath)
				continue
			}

			_, err = os.Stat(links[0])
			if err != nil {
				WarnLog(ctx, "%s doesn't exist or not accessible", links[0])
				continue
			}
			err = bindMount(storagePath, links[0])
			if err != nil {
				WarnLog(ctx, "Failed to bind mount %s to %s, %v", storagePath, links[0], err)
				continue
			}
		}
	}
	return nil
}
