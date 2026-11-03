import json
import os
import shutil
import subprocess
import tempfile

import pytest

# The GPU setup of the kepler cluster, without GPUs:
# - nvidia-cdi-refresh (nvidia-container-toolkit-base) writes the spec of the
#   driver to /var/run/cdi/nvidia.yaml: device nodes, driver libraries, hooks;
# - nvgpu-manager allocates GPUs with its own envs and mounts and requests the
#   CDI devices by an annotation of the container;
# - portoshim applies the spec to the container.

SPEC_PATH = '/var/run/cdi/nvidia.yaml'
TEST_ROOT = '/opt/cdi-test'
HOOK = os.path.join(TEST_ROOT, 'nvidia-cdi-hook')
HOOK_LOG = os.path.join(TEST_ROOT, 'hook.log')
LIBCUDA_DIR = os.path.join(TEST_ROOT, 'libcuda-dir')
DRIVER_LIB = os.path.join(TEST_ROOT, 'driver', 'libcuda.so.590.48.01')
LIB_DIR = '/usr/lib/x86_64-linux-gnu'

GPU = 'GPU-00000000-0000-0000-0000-000000000000'
ANNOTATION = 'cdi.k8s.io/nvgpu-manager_nvidia'
DEVICE_NODES = ('/dev/nvidiactl', '/dev/nvidia0')

# Plays nvidia-cdi-hook: gets the OCI state on stdin and edits the container
# root found in the bundle config.
HOOK_SCRIPT = '''#!/usr/bin/python3
import json
import os
import sys

state = json.load(sys.stdin)
with open(os.path.join(state['bundle'], 'config.json')) as f:
    root = json.load(f)['root']['path']
with open(%(log)r, 'a') as f:
    f.write(json.dumps({'args': sys.argv[1:], 'id': state['id'], 'root': root}) + '\\n')

if sys.argv[1] == 'create-symlinks':
    target, link = sys.argv[3].split('::')
    os.makedirs(os.path.dirname(root + link), exist_ok=True)
    os.symlink(target, root + link)
elif sys.argv[1] == 'update-ldcache':
    os.makedirs(root + '/etc/ld.so.conf.d', exist_ok=True)
    with open(root + '/etc/ld.so.conf.d/00-cdi-test.conf', 'w') as f:
        f.write(sys.argv[3] + '\\n')
else:
    sys.exit('unknown hook ' + sys.argv[1])
''' % {'log': HOOK_LOG}


def nvidia_spec():
    # The layout nvidia-ctk 1.20 generates on the B300 hosts.
    return {
        'cdiVersion': '0.5.0',
        'kind': 'nvidia.com/gpu',
        'devices': [{
            'name': GPU,
            'containerEdits': {
                'deviceNodes': [{'path': '/dev/nvidia0'}],
            },
        }],
        'containerEdits': {
            'env': ['NVIDIA_VISIBLE_DEVICES=void'],
            'deviceNodes': [{'path': '/dev/nvidiactl'}],
            'mounts': [{
                'hostPath': DRIVER_LIB,
                'containerPath': os.path.join(LIB_DIR, os.path.basename(DRIVER_LIB)),
                'options': ['ro', 'nosuid', 'nodev', 'rbind', 'rprivate'],
            }],
            'hooks': [
                {
                    'hookName': 'createContainer',
                    'path': HOOK,
                    'args': [
                        'nvidia-cdi-hook', 'create-symlinks',
                        '--link', os.path.basename(DRIVER_LIB) + '::' + os.path.join(LIB_DIR, 'libcuda.so.1'),
                    ],
                },
                {
                    'hookName': 'createContainer',
                    'path': HOOK,
                    'args': ['nvidia-cdi-hook', 'update-ldcache', '--folder', LIB_DIR],
                },
            ],
        },
    }


@pytest.fixture
def cdi_host():
    os.makedirs(os.path.dirname(DRIVER_LIB), exist_ok=True)
    os.makedirs(LIBCUDA_DIR, exist_ok=True)
    with open(DRIVER_LIB, 'w') as f:
        f.write('fake libcuda\n')
    with open(os.path.join(LIBCUDA_DIR, 'libcuda.so.1'), 'w') as f:
        f.write('nvgpu-manager libcuda\n')
    with open(HOOK, 'w') as f:
        f.write(HOOK_SCRIPT)
    os.chmod(HOOK, 0o755)
    # The VM has no GPUs: device nodes with the numbers of /dev/null.
    created = []
    for node in DEVICE_NODES:
        if not os.path.exists(node):
            os.mknod(node, 0o666 | 0o020000, os.makedev(1, 3))
            created.append(node)
    os.makedirs(os.path.dirname(SPEC_PATH), exist_ok=True)
    with open(SPEC_PATH, 'w') as f:
        json.dump(nvidia_spec(), f)

    yield

    os.unlink(SPEC_PATH)
    for node in created:
        os.unlink(node)
    shutil.rmtree(TEST_ROOT)


def crictl(*args, parse_json=False):
    cmd = ['crictl', '--runtime-endpoint', 'unix:///run/portoshim.sock', args[0]]
    if parse_json:
        cmd += ['-o', 'json']
    cmd += list(args[1:])
    print("+ " + " ".join(cmd), flush=True)
    result = subprocess.run(cmd, timeout=60, capture_output=True)
    if result.returncode != 0:
        print("STDERR:\n" + result.stderr.decode(), flush=True)
        raise subprocess.CalledProcessError(result.returncode, cmd, result.stdout, result.stderr)
    out = result.stdout.decode()
    return json.loads(out) if parse_json else out


def write_json_file(data):
    with tempfile.NamedTemporaryFile(mode='w', suffix='.json', delete=False) as f:
        json.dump(data, f)
    return f.name


def porto_get(name, prop):
    return subprocess.check_output(['portoctl', 'get', name, prop], text=True, timeout=10).strip()


def container_diagnostics(ct_id, info):
    print(json.dumps(info, indent=1), flush=True)
    for args in (
        ['portoctl', 'get', ct_id],
        ['portoctl', 'get', ct_id.split('/')[0]],
        ['journalctl', '--no-pager', '-o', 'short-precise', '-n', '200',
         '-u', 'portoshim', '-u', 'yandex-porto'],
        ['journalctl', '--no-pager', '-k', '-n', '50'],
    ):
        result = subprocess.run(args, capture_output=True, text=True, timeout=10, check=False)
        print("$ {}\n{}{}".format(' '.join(args), result.stdout, result.stderr), flush=True)


def gpu_pod(name, uid):
    return write_json_file({
        'metadata': {'name': name, 'namespace': 'default', 'uid': uid},
        'log_directory': '/tmp/{}-logs'.format(name),
    })


def gpu_container(devices=None):
    # What kubelet sends for a container with nvidia.com/gpu: the envs, mounts
    # and annotations of the nvgpu-manager allocation.
    config = {
        'metadata': {'name': 'gpu'},
        'image': {'image': 'registry.k8s.io/pause:3.9'},
        'command': ['/pause'],
        'log_path': 'gpu.log',
        'envs': [{'key': 'CUDA_LIB_PATH', 'value': '/libcuda-dir'}],
        'mounts': [{'container_path': '/libcuda-dir', 'host_path': LIBCUDA_DIR, 'readonly': True}],
    }
    if devices:
        config['annotations'] = {ANNOTATION: devices}
    return write_json_file(config)


class TestCDI():
    def test_gpu_container(self, cdi_host):
        pod_path = gpu_pod('cdi-gpu-pod', 'cdi-uid-1')
        ct_path = gpu_container('nvidia.com/gpu=' + GPU)
        pod_id = None
        try:
            pod_id = crictl('runp', pod_path).strip()
            ct_id = crictl('create', pod_id, ct_path, pod_path).strip()

            # Hooks run before the start, with the root of the created container.
            with open(HOOK_LOG) as f:
                calls = [json.loads(line) for line in f]
            assert [c['args'][0] for c in calls] == ['create-symlinks', 'update-ldcache']
            root = porto_get(ct_id, 'root_path')
            assert all(c['id'] == ct_id and c['root'] == root for c in calls)

            crictl('start', ct_id)
            info = crictl('inspect', ct_id, parse_json=True)
            if info['status']['state'] != 'CONTAINER_RUNNING':
                container_diagnostics(ct_id, info)
            assert info['status']['state'] == 'CONTAINER_RUNNING'

            # The container as its processes see it.
            proc = '/proc/{}'.format(porto_get(ct_id, 'root_pid'))
            with open(proc + '/environ') as f:
                env = dict(v.split('=', 1) for v in f.read().split('\0') if '=' in v)
            assert env['NVIDIA_VISIBLE_DEVICES'] == 'void'
            assert env['CUDA_LIB_PATH'] == '/libcuda-dir'

            ct_root = proc + '/root'
            for node in DEVICE_NODES:
                st = os.stat(ct_root + node)
                assert os.major(st.st_rdev) == 1 and os.minor(st.st_rdev) == 3, node

            # The driver library of the spec, the link made by the hook to it,
            # and the nvgpu-manager mount next to them.
            lib = os.path.join(LIB_DIR, os.path.basename(DRIVER_LIB))
            with open(ct_root + lib) as f:
                assert f.read() == 'fake libcuda\n'
            assert os.readlink(ct_root + os.path.join(LIB_DIR, 'libcuda.so.1')) == os.path.basename(DRIVER_LIB)
            with open(ct_root + os.path.join(LIB_DIR, 'libcuda.so.1')) as f:
                assert f.read() == 'fake libcuda\n'
            with open(ct_root + '/etc/ld.so.conf.d/00-cdi-test.conf') as f:
                assert f.read() == LIB_DIR + '\n'
            with open(ct_root + '/libcuda-dir/libcuda.so.1') as f:
                assert f.read() == 'nvgpu-manager libcuda\n'

            crictl('stop', ct_id)
        finally:
            if pod_id:
                crictl('stopp', pod_id)
                crictl('rmp', pod_id)
            os.unlink(pod_path)
            os.unlink(ct_path)

    def test_no_annotation(self, cdi_host):
        # Pods without the annotation, like YT ones before CDI, get nothing of the spec.
        pod_path = gpu_pod('cdi-plain-pod', 'cdi-uid-2')
        ct_path = gpu_container()
        pod_id = None
        try:
            pod_id = crictl('runp', pod_path).strip()
            ct_id = crictl('create', pod_id, ct_path, pod_path).strip()
            crictl('start', ct_id)

            proc = '/proc/{}'.format(porto_get(ct_id, 'root_pid'))
            with open(proc + '/environ') as f:
                assert 'NVIDIA_VISIBLE_DEVICES=' not in f.read()
            assert not os.path.exists(proc + '/root' + os.path.join(LIB_DIR, 'libcuda.so.1'))
            assert not os.path.exists(HOOK_LOG)

            crictl('stop', ct_id)
        finally:
            if pod_id:
                crictl('stopp', pod_id)
                crictl('rmp', pod_id)
            os.unlink(pod_path)
            os.unlink(ct_path)

    def test_unknown_device(self, cdi_host):
        # A GPU missing from the spec, e.g. a spec not refreshed after a GPU
        # replacement, fails the container instead of starting it without the GPU.
        pod_path = gpu_pod('cdi-unknown-pod', 'cdi-uid-3')
        ct_path = gpu_container('nvidia.com/gpu=GPU-ffffffff-ffff-ffff-ffff-ffffffffffff')
        pod_id = None
        try:
            pod_id = crictl('runp', pod_path).strip()
            with pytest.raises(subprocess.CalledProcessError) as error:
                crictl('create', pod_id, ct_path, pod_path)
            assert 'unresolvable CDI device' in error.value.stderr.decode()
        finally:
            if pod_id:
                crictl('stopp', pod_id)
                crictl('rmp', pod_id)
            os.unlink(pod_path)
            os.unlink(ct_path)

    def test_failed_hook(self, cdi_host):
        # A failed hook fails the container and leaves nothing behind.
        spec = nvidia_spec()
        spec['containerEdits']['hooks'][0]['args'][1] = 'no-such-hook'
        with open(SPEC_PATH, 'w') as f:
            json.dump(spec, f)

        pod_path = gpu_pod('cdi-hook-pod', 'cdi-uid-4')
        ct_path = gpu_container('nvidia.com/gpu=' + GPU)
        pod_id = None
        try:
            pod_id = crictl('runp', pod_path).strip()
            with pytest.raises(subprocess.CalledProcessError) as error:
                crictl('create', pod_id, ct_path, pod_path)
            assert 'CDI createContainer hook' in error.value.stderr.decode()

            containers = crictl('ps', '-a', parse_json=True).get('containers', [])
            assert not [c for c in containers if c.get('podSandboxId') == pod_id]
        finally:
            if pod_id:
                crictl('stopp', pod_id)
                crictl('rmp', pod_id)
            os.unlink(pod_path)
            os.unlink(ct_path)
