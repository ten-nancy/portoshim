package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	bindMountSuffixes = []string{
		"/var/run/secrets/kubernetes.io/serviceaccount",
	}

	tmpfsDirs map[string]string
)

func bindMount(source, dest string) error {
	return unix.Mount(source, dest, "", uintptr(unix.MS_BIND|unix.MS_REC), "")
}

func init() {
	tmpfsDirs = make(map[string]string, 0)
}

func removeSpecificVolumes(ctx context.Context, id, linkedVolumes string) error {
	vols := strings.Split(linkedVolumes, ";")
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
			DebugLog(ctx, "Suffix found")
			tmpfsDirKey := filepath.Join(id, links[1])
			tmpfs, ok := tmpfsDirs[tmpfsDirKey]
			if ok {
				err := os.RemoveAll(tmpfs)
				delete(tmpfsDirs, tmpfsDirKey)
				if err != nil {
					return fmt.Errorf("Failed to remove %s %w", tmpfs, err)
				}
			} else {
				DebugLog(ctx, "No entry %v in tmpfsDirs %+v ", tmpfsDirKey, tmpfsDirs)
			}
		}
	}
	return nil
}
