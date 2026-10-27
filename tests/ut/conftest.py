import json
import subprocess
import time

import pytest
import yatest.common


def run(args, **kwargs):
    print("+ '" + "' '".join(args) + "'", flush=True)
    subprocess.check_call(args, **kwargs)


def log_portod_version():
    args = ['/usr/sbin/portod', '--version']
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=10, check=False)
        output = result.stdout + result.stderr
        print(
            "$ {}\n{}[exit code: {}]".format(' '.join(args), output, result.returncode),
            flush=True,
        )
    except subprocess.TimeoutExpired as error:
        print("$ {}\nVersion command timed out: {}".format(' '.join(args), error), flush=True)


SERVICES = ('yandex-porto', 'portoshim', 'kubelet')
PAUSE_IMAGE = 'registry.k8s.io/pause:3.9'


def verify_preloaded_images():
    args = ['portoctl', 'docker-images', '-P', '/place']
    output = subprocess.check_output(args, text=True, timeout=15)
    print("$ {}\n{}".format(' '.join(args), output), flush=True)
    cached = set()
    for line in output.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[0] != 'ID':
            cached.add(parts[1])
    if PAUSE_IMAGE not in cached:
        raise RuntimeError(
            "Required image {} is not cached; rebuild the test image with "
            "43_preload_images.sh. Tests do not download images.".format(PAUSE_IMAGE)
        )


def service_diagnostics():
    diagnostics = []
    for args in (
        ['systemctl', '--no-pager', '--full', 'status', *SERVICES],
        ['journalctl', '--no-pager', '-o', 'short-precise', '-n', '200',
         '-u', 'yandex-porto', '-u', 'portoshim', '-u', 'kubelet'],
        ['journalctl', '--no-pager', '-k', '-n', '100'],
    ):
        try:
            result = subprocess.run(args, capture_output=True, text=True, timeout=10, check=False)
            diagnostics.append(
                "$ {}\n{}{}".format(
                    ' '.join(args),
                    result.stdout,
                    result.stderr,
                )
            )
        except subprocess.TimeoutExpired:
            diagnostics.append("$ {}\nDiagnostics timed out".format(' '.join(args)))
    output = '\n'.join(diagnostics)
    print(output, flush=True)
    return output


def wait_ready(service, probe, timeout=60):
    deadline = time.monotonic() + timeout
    last_output = ''
    while time.monotonic() < deadline:
        try:
            active = subprocess.run(
                ['systemctl', 'is-active', '--quiet', service], timeout=5
            )
            if active.returncode == 0:
                result = subprocess.run(probe, capture_output=True, text=True, timeout=5)
                last_output = result.stdout + result.stderr
                if result.returncode == 0:
                    return
        except subprocess.TimeoutExpired as error:
            stdout = error.stdout or ''
            stderr = error.stderr or ''
            if isinstance(stdout, bytes):
                stdout = stdout.decode(errors='replace')
            if isinstance(stderr, bytes):
                stderr = stderr.decode(errors='replace')
            last_output = "{}\n{}".format(stdout, stderr).strip() or str(error)
        time.sleep(0.5)
    raise RuntimeError(
        "{} did not become ready. Probe: {}\nProbe output: {}".format(
            service,
            ' '.join(probe),
            last_output,
        )
    )


@pytest.fixture(scope='session', autouse=True)
def prepare_env():
    # Porto is installed in the test image from the yandex-porto deb package.
    # Log its version before starting the service so failures retain provenance.
    log_portod_version()

    # 2. Install portoshim binary
    portoshim_bin = yatest.common.binary_path(
        'infra/porto/plugins/portoshim/cmd/portoshim/portoshim'
    )
    run(['cp', portoshim_bin, '/usr/sbin/portoshim'])
    run(['chmod', '+x', '/usr/sbin/portoshim'])

    # systemctl start and a socket file alone do not guarantee API readiness.
    run(['systemctl', 'daemon-reload'])
    try:
        run(['systemctl', 'start', 'yandex-porto'], timeout=60)
        wait_ready('yandex-porto', ['portoctl', 'list'])
        verify_preloaded_images()
        run(['systemctl', 'start', 'portoshim'], timeout=60)
        wait_ready('portoshim', [
            'crictl', '--runtime-endpoint', 'unix:///run/portoshim.sock',
            '--timeout', '3s', 'version',
        ])
    except Exception as error:
        diagnostics = service_diagnostics()
        raise RuntimeError("{}\n\n{}".format(error, diagnostics)) from error

    # 5. Deploy static pod manifest and start kubelet
    static_pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": "test-static-pod",
            "namespace": "default",
        },
        "spec": {
            "containers": [{
                "name": "pause",
                "image": PAUSE_IMAGE,
                "imagePullPolicy": "Never",
            }],
        },
    }
    with open('/etc/kubernetes/manifests/test-static-pod.json', 'w') as f:
        json.dump(static_pod, f)

    try:
        run(['systemctl', 'start', 'kubelet'], timeout=60)
        wait_ready('kubelet', [
            'python3', '-c',
            'import urllib.request; '
            'urllib.request.urlopen("http://127.0.0.1:10248/healthz", timeout=3).read()',
        ])
        for service in SERVICES:
            run(['systemctl', 'is-active', '--quiet', service], timeout=5)
    except Exception as error:
        diagnostics = service_diagnostics()
        raise RuntimeError("{}\n\n{}".format(error, diagnostics)) from error

    try:
        yield
    finally:
        service_diagnostics()
        # Bound cleanup so a stuck daemon cannot consume the whole suite timeout.
        for service in reversed(SERVICES):
            try:
                run(['systemctl', 'stop', service], timeout=20)
            except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
                print("Failed to stop {}: {}".format(service, error), flush=True)
