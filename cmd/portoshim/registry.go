package main

import (
	"os"
	"strings"

	"go.uber.org/zap"
)

type RegistryInfo struct {
	Host        string `yaml:"Host"`
	AuthToken   string `yaml:"AuthToken"`
	AuthPath    string `yaml:"AuthPath"`
	AuthService string `yaml:"AuthService"`
}

const (
	defaultDockerRegistry = "registry-1.docker.io"
)

var (
	KnownRegistries = map[string]*RegistryInfo{
		defaultDockerRegistry: &RegistryInfo{
			Host: defaultDockerRegistry,
		},
		"quay.io": &RegistryInfo{
			Host:     "quay.io",
			AuthPath: "https://quay.io/v2/auth",
		},
	}
)

func InitKnownRegistries() error {
	for _, info := range Cfg.Images.Registries {
		zap.S().Infof("add registry from config %+v", info)
		KnownRegistries[info.Host] = info
	}

	for host, registry := range KnownRegistries {
		if strings.HasPrefix(registry.AuthToken, "file:") {
			authTokenPath := registry.AuthToken[5:]
			// if file doesn't exist then auth token is empty
			_, err := os.Stat(authTokenPath)
			if err != nil {
				if os.IsNotExist(err) {
					registry.AuthToken = ""
				} else {
					return err
				}
			} else {
				content, err := os.ReadFile(authTokenPath)
				if err != nil {
					return err
				}
				registry.AuthToken = strings.TrimSpace(string(content))
			}
			KnownRegistries[host] = registry
		}
	}
	return nil
}

func GetImageRegistry(name string) *RegistryInfo {
	host := defaultDockerRegistry

	slashPos := strings.Index(name, "/")
	if slashPos > -1 {
		host = name[:slashPos]
	}

	if registry, ok := KnownRegistries[host]; ok {
		return registry
	}

	return nil
}
