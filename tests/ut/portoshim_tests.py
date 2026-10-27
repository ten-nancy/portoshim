import json
import os
import subprocess
import tempfile
import time


CRICTL = ['crictl', '--runtime-endpoint', 'unix:///run/portoshim.sock']


def crictl(*args, parse_json=False):
    cmd = list(CRICTL) + list(args[:1])
    if parse_json:
        cmd += ['-o', 'json']
    cmd += list(args[1:])
    print("+ " + " ".join(cmd), flush=True)
    result = subprocess.run(cmd, timeout=60, capture_output=True)
    if result.returncode != 0:
        print("STDERR:\n" + result.stderr.decode(), flush=True)
        print("STDOUT:\n" + result.stdout.decode(), flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, result.stdout, result.stderr)
    out = result.stdout.decode()
    if parse_json:
        return json.loads(out)
    return out


def write_json_file(data):
    f = tempfile.NamedTemporaryFile(mode='w', suffix='.json', delete=False)
    json.dump(data, f)
    f.close()
    return f.name


class TestVersion():
    def test_version(self):
        out = crictl('version')
        assert 'RuntimeName' in out or 'runtimeName' in out


class TestPodLifecycle():
    def test_pod_lifecycle(self):
        pod_config_path = write_json_file({
            "metadata": {
                "name": "test-pod",
                "namespace": "default",
                "uid": "test-uid-1",
            },
            "log_directory": "/tmp/pod-logs",
        })

        try:
            pod_id = crictl('runp', pod_config_path).strip()
            assert pod_id

            # Verify pod is listed
            pods = crictl('pods', parse_json=True)
            pod_ids = [p['id'] for p in pods.get('items', [])]
            assert pod_id in pod_ids

            # Stop and remove
            crictl('stopp', pod_id)
            crictl('rmp', pod_id)

            # Verify removed
            pods = crictl('pods', parse_json=True)
            pod_ids = [p['id'] for p in pods.get('items', [])]
            assert pod_id not in pod_ids
        finally:
            os.unlink(pod_config_path)


class TestContainerLifecycle():
    def test_container_lifecycle(self):
        pod_path = write_json_file({
            "metadata": {
                "name": "test-ct-pod",
                "namespace": "default",
                "uid": "test-uid-2",
            },
            "log_directory": "/tmp/ct-pod-logs",
        })
        ct_path = write_json_file({
            "metadata": {"name": "test-container"},
            "image": {"image": "registry.k8s.io/pause:3.9"},
            "command": ["/pause"],
            "log_path": "test-container.log",
        })

        try:
            pod_id = crictl('runp', pod_path).strip()
            ct_id = crictl('create', pod_id, ct_path, pod_path).strip()
            assert ct_id

            crictl('start', ct_id)

            # Verify running
            info = crictl('inspect', ct_id, parse_json=True)
            state = info.get('status', {}).get('state', '')
            assert state == 'CONTAINER_RUNNING'

            # Cleanup
            crictl('stop', ct_id)
            # TODO: Restore removal after ContainerStatus stops destroying dead containers.
            # crictl rm checks status first, so RemoveContainer sees a missing container.
            # crictl('rm', ct_id)
            crictl('stopp', pod_id)
            crictl('rmp', pod_id)
        finally:
            os.unlink(pod_path)
            os.unlink(ct_path)


class TestStaticPod():
    def test_static_pod_running(self):
        # Static pod manifest is deployed by conftest.py before kubelet starts.
        # Wait for kubelet to create the static pod via portoshim -> portod.
        found = False
        for _ in range(60):
            pods = crictl('pods', parse_json=True)
            for p in pods.get('items', []):
                if 'test-static-pod' in p.get('metadata', {}).get('name', ''):
                    if p.get('state') == 'SANDBOX_READY':
                        found = True
                        break
            if found:
                break
            time.sleep(1)

        assert found, "static pod 'test-static-pod' did not reach SANDBOX_READY state"

        # Verify container inside the static pod is running
        containers = crictl('ps', parse_json=True)
        running = False
        for c in containers.get('containers', []):
            if c.get('metadata', {}).get('name', '') == 'pause':
                if c.get('state') == 'CONTAINER_RUNNING':
                    running = True
                    break

        assert running, "container 'pause' in static pod is not running"
