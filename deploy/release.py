#!/usr/bin/env python3
"""Operador del VPS. Descarga únicamente artefactos del workflow autorizado."""
import datetime
import fcntl
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time
import urllib.request
import urllib.parse
import zipfile

ROOT = pathlib.Path('/opt/sharedata')
K = ['/usr/local/bin/k3s', 'kubectl', '-n', 'sharedata']
MAX_ARTIFACT = 256 * 1024 * 1024

class SafeRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        if urllib.parse.urlsplit(new_url).scheme != 'https':
            raise RuntimeError('El artefacto requiere transporte HTTPS.')
        redirected = super().redirect_request(request, response, code, message, headers, new_url)
        if urllib.parse.urlsplit(new_url).hostname != 'api.github.com':
            redirected.remove_header('Authorization')
        return redirected

ARTIFACT_OPENER = urllib.request.build_opener(SafeRedirect())

def api(path, token):
    request = urllib.request.Request('https://api.github.com/' + path, headers={
        'Authorization': 'Bearer ' + token, 'Accept': 'application/vnd.github+json',
        'User-Agent': 'sharedata-release', 'X-GitHub-Api-Version': '2022-11-28'})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)

def command(args):
    return subprocess.check_output(args, text=True).strip()

def healthy(sha):
    for url in ('http://127.0.0.1:8844/readyz', 'https://sd.server.gt/readyz'):
        for attempt in range(10):
            try:
                result = command(['/usr/bin/curl', '--fail', '--silent', '--show-error',
                    '--max-time', '5', '-D', '-', url])
                headers, _, body = result.replace('\r\n', '\n').partition('\n\n')
                if 'x-sharedata-revision: ' + sha in headers.lower() and body.strip() == 'ok':
                    break
            except subprocess.CalledProcessError:
                pass
            if attempt == 9:
                raise RuntimeError('La revisión publicada no coincide con el build sano.')
            time.sleep(1)


def release(run_id, sha, token):
    settings = json.loads((ROOT / 'deploy-settings.json').read_text())
    repo = settings['repository']
    run = api(f'repos/{repo}/actions/runs/{run_id}', token)
    if (run['event'] not in ('push', 'workflow_dispatch') or
        run['head_branch'] not in settings['branches'] or run['head_sha'] != sha or
        run['path'] != '.github/workflows/build.yml' or
        run['head_repository']['full_name'].lower() != repo.lower()):
        raise RuntimeError('El run no pertenece a una entrega autorizada.')
    branch = urllib.parse.quote(run['head_branch'], safe='')
    head = api(f'repos/{repo}/git/ref/heads/{branch}', token)
    if head['object']['sha'] != sha:
        raise RuntimeError('Build anterior a la revisión actual; no se despliega.')
    artifacts = api(f'repos/{repo}/actions/runs/{run_id}/artifacts', token)['artifacts']
    artifact = next((x for x in artifacts if x['name'] == 'sharedata-image-' + sha and not x['expired']), None)
    if artifact is None or artifact['size_in_bytes'] > MAX_ARTIFACT:
        raise RuntimeError('Artefacto ausente o de tamaño inesperado.')
    directory = ROOT / 'releases' / sha
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    archive = directory / 'image.zip'
    tar = directory / 'sharedata.tar'
    old = json.loads(command(K + ['get', 'deployment', 'sharedata', '-o', 'json']))
    previous = next(c['image'] for c in old['spec']['template']['spec']['containers'] if c['name'] == 'sharedata')
    image = settings['image_repository'] + ':' + sha
    changed = False
    try:
        request = urllib.request.Request(artifact['archive_download_url'], headers={
            'Authorization': 'Bearer ' + token, 'User-Agent': 'sharedata-release'})
        with ARTIFACT_OPENER.open(request, timeout=60) as response, archive.open('wb') as output:
            size = 0
            while block := response.read(1024 * 1024):
                size += len(block)
                if size > MAX_ARTIFACT:
                    raise RuntimeError('Artefacto supera el límite.')
                output.write(block)
        digest = 'sha256:' + hashlib.sha256(archive.read_bytes()).hexdigest()
        if artifact.get('digest') and artifact['digest'] != digest:
            raise RuntimeError('El checksum del artefacto no coincide.')
        with zipfile.ZipFile(archive) as zipped:
            entry = zipped.getinfo('sharedata.tar')
            if entry.file_size > MAX_ARTIFACT:
                raise RuntimeError('Imagen supera el límite.')
            with zipped.open(entry) as source, tar.open('wb') as output:
                shutil.copyfileobj(source, output)
        command(['/usr/local/bin/k3s', 'ctr', 'images', 'import', str(tar)])
        changed = True
        candidate = {'spec': {'replicas': 2, 'minReadySeconds': 5,
            'progressDeadlineSeconds': 180,
            'strategy': {'type': 'RollingUpdate', 'rollingUpdate': {'maxUnavailable': 0, 'maxSurge': 1}},
            'template': {'spec': {'terminationGracePeriodSeconds': 30,
                'containers': [{'name': 'sharedata', 'image': image}]}}}}
        command(K + ['patch', 'deployment/sharedata', '--type=strategic', '-p', json.dumps(candidate)])
        command(K + ['rollout', 'status', 'deployment/sharedata', '--timeout=180s'])
        healthy(sha)
        state = {'repository': repo, 'branch': run['head_branch'], 'sha': sha,
            'actions_run': int(run_id), 'image': image, 'previous_image': previous,
            'domain': 'https://sd.server.gt', 'namespace': 'sharedata',
            'verified_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'artifact_digest': digest, 'ready': True}
        state_path = ROOT / 'current.json'
        if state_path.exists():
            (directory / 'previous.json').write_text(state_path.read_text())
        temporary = ROOT / 'current.json.next'
        temporary.write_text(json.dumps(state, indent=2) + '\n')
        temporary.replace(state_path)
        print('Despliegue verificado: ' + sha)
    except Exception:
        if changed:
            print('La revisión nueva falló; restaurando la imagen anterior.', file=sys.stderr)
            command(K + ['patch', 'deployment/sharedata', '--type=json', '-p', json.dumps([{'op':'replace','path':'/spec','value':old['spec']}])])
            command(K + ['rollout', 'status', 'deployment/sharedata', '--timeout=180s'])
            command(['/usr/bin/curl', '--fail', '--silent', '--show-error', '--max-time', '15', 'http://127.0.0.1:8844/readyz'])
            print('Imagen anterior restaurada.', file=sys.stderr)
        raise
    finally:
        archive.unlink(missing_ok=True)
        tar.unlink(missing_ok=True)

if __name__ == '__main__':
    if len(sys.argv) != 3 or not re.fullmatch(r'[0-9]+', sys.argv[1]) or not re.fullmatch(r'[0-9a-f]{40}', sys.argv[2]):
        raise SystemExit('Uso: sharedata-release RUN_ID COMMIT_SHA; token de Actions por stdin.')
    token = sys.stdin.read().strip()
    if not token:
        raise SystemExit('Falta la autorización temporal de Actions.')
    os.environ['PATH'] = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
    with pathlib.Path('/run/lock/sharedata-release.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            release(sys.argv[1], sys.argv[2], token)
        except Exception as error:
            # No registrar headers, respuestas de GitHub, tokens ni URLs firmadas.
            print('Error de despliegue: ' + (str(error) if isinstance(error, RuntimeError) else type(error).__name__), file=sys.stderr)
            raise SystemExit(1)
