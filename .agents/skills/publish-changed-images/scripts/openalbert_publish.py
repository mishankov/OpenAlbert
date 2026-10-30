#!/usr/bin/env python3
"""Plan an OpenAlbert release; --execute verifies and publishes selected images."""
import argparse
import concurrent.futures
import importlib.util
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import sys

COMPONENTS = ('openalbert', 'stt', 'log-gateway', 'minio', 'minio-init', 'backup')
ARCHES = ('amd64', 'arm64')
INPUTS = {
    'openalbert': ('apps/backend/', 'services/backup/', 'go.mod', 'go.sum'),
    'backup': ('services/backup/', 'go.mod', 'go.sum'),
    'stt': ('services/stt/', 'go.mod', 'go.sum'),
    'log-gateway': ('services/log-gateway/', 'go.mod', 'go.sum'),
    'minio': ('services/minio/Dockerfile',),
    'minio-init': ('services/minio/Dockerfile', 'services/minio/init.sh'),
}
GLOBAL_INPUTS = ('.dockerignore', 'deploy/docker-bake.hcl')
DEPLOYMENT = ('openalbert', 'log-gateway', 'minio', 'minio-init')


def command(repo, args):
    return subprocess.check_output(args, cwd=repo, text=True, timeout=120).strip()


def affects(component, path):
    return any(path.startswith(item) if item.endswith('/') else path == item
               for item in INPUTS[component] + GLOBAL_INPUTS)


def published(repo, prefix, component, tag='master'):
    ref = prefix + '/' + component + ':' + tag
    config = json.loads(command(repo, ['docker', 'buildx', 'imagetools', 'inspect', ref, '--format', '{{json .Image}}']))
    manifest = json.loads(command(repo, ['docker', 'buildx', 'imagetools', 'inspect', ref, '--raw']))
    platforms = {}
    for arch in ARCHES:
        platform = 'linux/' + arch
        entries = [item for item in manifest.get('manifests', [])
                   if item.get('platform', {}).get('os') == 'linux'
                   and item.get('platform', {}).get('architecture') == arch]
        if len(entries) != 1 or platform not in config:
            raise RuntimeError(ref + ': missing or ambiguous ' + platform)
        platforms[arch] = {'digest': entries[0]['digest'], 'revision':
                          config[platform].get('config', {}).get('Labels', {}).get('org.opencontainers.image.revision')}
    return {'reference': ref, 'manifest': manifest, 'platforms': platforms}


def plan(repo, prefix):
    revision = command(repo, ['git', 'rev-parse', 'HEAD'])
    if command(repo, ['git', 'status', '--porcelain']):
        raise RuntimeError('Use a clean checkout for the image plan')
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
        images = dict(zip(COMPONENTS, pool.map(lambda name: published(repo, prefix, name), COMPONENTS)))
    diffs = {}
    selected = []
    for name, image in images.items():
        reasons = []
        for old in sorted({state['revision'] or '' for state in image['platforms'].values()}):
            if not re.fullmatch(r'[0-9a-fA-F]{40}|[0-9a-fA-F]{64}', old):
                reasons.append('Published revision label is missing or not a full commit ID')
                continue
            if old not in diffs:
                try:
                    # Zero-delimited names preserve spaces and renamed input paths.
                    raw = command(repo, ['git', 'diff', '--name-only', '--no-renames', '-z', old, revision, '--'])
                    diffs[old] = raw.split('\0') if raw else []
                except subprocess.CalledProcessError:
                    diffs[old] = None
            if diffs[old] is None:
                reasons.append('Published commit is unavailable locally: ' + old)
            else:
                reasons.extend(path for path in diffs[old] if affects(name, path))
        image['reasons'] = sorted(set(reasons))
        if reasons:
            selected.append(name)
    return {'revision': revision, 'prefix': prefix, 'selected': selected, 'images': images}


def verify(repo, data):
    for name, before in data['images'].items():
        after = published(repo, data['prefix'], name)
        if name in data['selected']:
            immutable = published(repo, data['prefix'], name, 'sha-' + data['revision'])
            if after['manifest'] != immutable['manifest']:
                raise RuntimeError(name + ': master and immutable tags differ')
            if any(state['revision'] != data['revision'] for state in after['platforms'].values()):
                raise RuntimeError(name + ': published revision does not match verified source')
        elif after['manifest'] != before['manifest']:
            raise RuntimeError(name + ': an unchanged component manifest changed during publication')


def execute(repo, data):
    spec = importlib.util.spec_from_file_location('openalbert_local', repo / 'tools/local.py')
    local = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(local)
    local.COMPONENTS = tuple(data['selected'])
    os.environ['OPENALBERT_IMAGE_PREFIX'] = data['prefix']

    class SelectedRun(local.Run):
        def clean_checkout(self, master=False):
            super().clean_checkout(master=master)
            if self.data['revision'] != data['revision']:
                raise RuntimeError('HEAD changed after the image plan')

        def images(self, arch, deployment=False):
            env = dict(self.env, LOCAL_REPOSITORY=self.repository, ARCH=arch, REVISION=data['revision'],
                       DOCKER_DEFAULT_PLATFORM='linux/' + arch)
            self.command('images-' + arch, ['docker', 'buildx', 'bake', '-f', 'deploy/docker-bake.hcl',
                         *data['selected'], '--load', '--metadata-file', self.directory / ('images-' + arch + '.json')],
                         env=env, timeout=7200)
            images = {name: self.repository + ':' + name + '-' + arch for name in data['selected']}
            needed = set(DEPLOYMENT if deployment else ())
            if 'minio-init' in data['selected']:
                needed.add('minio')
            for name in sorted(needed - images.keys()):
                ref = data['prefix'] + '/' + name + '@' + data['images'][name]['platforms'][arch]['digest']
                self.command('reuse-' + name + '-' + arch,
                             ['docker', 'pull', '--platform', 'linux/' + arch, ref], env=env, timeout=600)
                images[name] = ref
            self.data['images'][arch] = images
            self.save()
            if 'minio' in images:
                env['MINIO_SMOKE_IMAGE'] = images['minio']
            for name in data['selected']:
                self.command('smoke-' + name + '-' + arch,
                             ['bash', 'tools/smoke/smoke-image.sh', name, images[name]], env=env, timeout=900)
            if deployment:
                args = [sys.executable, 'tools/smoke/release-smoke.py']
                for name in DEPLOYMENT:
                    args.extend(['--image', name + '=' + images[name]])
                self.command('deployment-' + arch, args, env=env, timeout=1200)

    def cancel(signum, frame):
        raise KeyboardInterrupt('Signal ' + str(signum))

    signal.signal(signal.SIGTERM, cancel)
    run = SelectedRun('publish:changed-images')
    run.data['image_plan'] = data
    code = 0
    try:
        run.publish()
        verify(repo, data)
        run.data['status'] = 'passed'
    except KeyboardInterrupt as error:
        run.data.update(status='cancelled', failure=str(error) or 'Interrupted')
        code = 130
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
        run.data.update(status='failed', failure=str(error))
        print(str(error), file=sys.stderr)
        code = 1
    finally:
        run.save()
        temporary = local.REPORTS / (run.id + '.latest')
        temporary.write_text(run.id + '\n')
        temporary.replace(local.REPORTS / 'latest.txt')
        print(f"{run.data['status']}: {run.data['command']}\nReport: {run.directory / 'summary.md'}")
    return code


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--prefix', default='ghcr.io/mishankov/openalbert')
    parser.add_argument('--execute', action='store_true', help='Run verification, builds, and registry publication')
    args = parser.parse_args()
    repo = args.repo.resolve()
    if not re.fullmatch(r'[a-z0-9][a-z0-9.-]*(?::[0-9]+)?/[a-z0-9][a-z0-9_./-]*', args.prefix):
        parser.error('Use a lowercase registry/owner/repository prefix')
    try:
        data = plan(repo, args.prefix)
        print(json.dumps(data, indent=2), flush=True)
        if not args.execute or not data['selected']:
            return 0
        return execute(repo, data)
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
