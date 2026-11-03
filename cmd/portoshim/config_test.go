package main

import (
	"html/template"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var defaultConfigToTest = `
Portoshim:
    ConfigPath: {{.ConfigPath}} 
    Socket:     /run/portoshim.sock
    LogsDir:    /var/log/portoshim
    VolumesDir: {{.VolumesDir}}
Porto:
    RuntimeName: porto
    Socket:      /run/portod.socket
    SocketTimeout: 5m
    ImagesDir:   /place/porto_docker
CNI:
    ConfDir:  /etc/cni/net.d
    BinDir:   /opt/cni/bin
    NetnsDir: /var/run/netns
StreamingServer:
    Address: "[::]"
    Port:    7255
Images:
    PauseImage: {{.PauseImage}} 
    Place: {{.Place}}
`

const (
	pauseImage = "myregistry.k8s.io/pause:3.7"
	testPlace  = "/test_place"
)

func initFakeConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "portoshimconf-")
	if err != nil {
		t.Fatalf("Failed to MkdirTemp %v", err)
	}

	t.Logf("Created temp dir: %s\n", tmpDir)

	// Optional: Defer cleanup
	defer func() {
		err := os.RemoveAll(tmpDir)
		if err != nil {
			t.Logf("Warning: could not remove temp dir: %s\n", tmpDir)
		}
	}()

	configPath := tmpDir + "/portoshim.yaml"
	tmpl, err := template.New("profile").Parse(defaultConfigToTest)
	if err != nil {
		t.Fatalf("Failed to parse %s, %v", defaultConfigToTest, err)
	}
	portoshimInHome, err := os.UserHomeDir()
	if err != nil {
		t.Logf("Failed to get home dir")
		portoshimInHome = "/tmp"
	}
	portoshimInHome = filepath.Join(portoshimInHome, "portoshim_volumes")
	profile := struct {
		PauseImage string
		ConfigPath string
		Place      string
		VolumesDir string
	}{
		PauseImage: pauseImage,
		ConfigPath: configPath,
		Place:      testPlace,
		VolumesDir: portoshimInHome,
	}
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("Failed to open %s, %v", configPath, err)
	}

	defer file.Close()

	writer := io.Writer(file)
	if err = tmpl.Execute(writer, profile); err != nil {
		t.Fatalf("Failed to execute template parser %v", err)
	}

	if err = InitConfig(configPath); err != nil {
		t.Fatalf("Failed to init config %v", err)
	}
}

func TestInitConfig(t *testing.T) {
	initFakeConfig(t)
	if Cfg.Images.PauseImage != pauseImage {
		t.Fatalf("Pause image not equal")
	}
}

func TestInitConfigCDISpecDirs(t *testing.T) {
	initFakeConfig(t)
	if want := []string{"/etc/cdi", "/var/run/cdi"}; !reflect.DeepEqual(Cfg.CDI.SpecDirs, want) {
		t.Fatalf("default CDI spec dirs %v, want %v", Cfg.CDI.SpecDirs, want)
	}

	path := filepath.Join(t.TempDir(), "portoshim.yaml")
	if err := os.WriteFile(path, []byte("CDI:\n    SpecDirs:\n        - /opt/cdi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitConfig(path); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/cdi"}; !reflect.DeepEqual(Cfg.CDI.SpecDirs, want) {
		t.Fatalf("configured CDI spec dirs %v, want %v", Cfg.CDI.SpecDirs, want)
	}
}
