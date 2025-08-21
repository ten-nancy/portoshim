package main

import (
	"path"
	"testing"
)

const portoName = "ISS-AGENT--alexperevalov-portoshim-stage-48/box/ps-87378-0"

// for portoAPI testing purpose
func setCFG() {
	InitConfig("")
	Cfg.Porto.ParentContainer = portoName
}

func TestIsPod(t *testing.T) {
	setCFG()

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
	setCFG()

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
		},
	} {
		t.Logf("Test #%d", ntest)
		if isContainer(test.cntname, portoName) != test.expected {
			t.Fatalf("test: %v cnt name: %v expected %v, PORTO_NAME: %v", test.name, test.cntname, test.expected, portoName)
		}
	}
}
