"""Ejecutar en el VPS: crea base/rol/secret propios. Nunca imprime credenciales."""
import json
import pathlib
import secrets
import subprocess
import urllib.parse

base = pathlib.Path('/opt/sharedata')
base.mkdir(mode=0o700, exist_ok=True)
path = base / 'credentials.json'
if path.exists():
    config = json.loads(path.read_text())
else:
    password = secrets.token_hex(24)
    config = {
        'DATABASE_URL': f'postgres://sharedata_app:{password}@postgres.dev-databases.svc.cluster.local:5432/sharedata_dev?sslmode=disable',
        'JWT_SECRET': secrets.token_hex(48),
        'TLS': 'false',
    }
    path.write_text(json.dumps(config, indent=2) + '\n')
    path.chmod(0o600)
password = urllib.parse.urlsplit(config['DATABASE_URL']).password
# Contraseña generada localmente: solo hex, nunca datos del usuario en SQL.
assert password and all(c in '0123456789abcdef' for c in password)
psql = ['k3s', 'kubectl', '-n', 'dev-databases', 'exec', '-i', 'postgres-0', '--', 'psql', '-U', 'dev_admin', '-d', 'development', '-v', 'ON_ERROR_STOP=1', '-At']
sql = f"""
DO $body$ BEGIN
 IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'sharedata_app') THEN
  CREATE ROLE sharedata_app LOGIN PASSWORD '{password}' NOSUPERUSER NOCREATEDB NOCREATEROLE;
 END IF;
END $body$;
SELECT 'CREATE DATABASE sharedata_dev OWNER sharedata_app' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='sharedata_dev')\gexec
REVOKE ALL ON DATABASE sharedata_dev FROM PUBLIC;
"""
r = subprocess.run(psql, input=sql, capture_output=True, text=True)
if r.returncode:
    raise SystemExit('Falló la creación de la base/rol; revisar PostgreSQL sin exponer credenciales.')
subprocess.run(['k3s', 'kubectl', 'create', 'namespace', 'sharedata'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
secret = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'sharedata-config', 'namespace': 'sharedata'}, 'type': 'Opaque', 'stringData': config}
subprocess.run(['k3s', 'kubectl', 'apply', '-f', '-'], input=json.dumps(secret), text=True, check=True)
unit = '''[Unit]
Description=ShareData origin for Cloudflare Tunnel
After=k3s.service
Requires=k3s.service
[Service]
ExecStart=/usr/local/bin/k3s kubectl -n sharedata port-forward --address=127.0.0.1 service/sharedata 8844:8844
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
'''
pathlib.Path('/etc/systemd/system/sharedata-forward.service').write_text(unit)
subprocess.run(['systemctl', 'daemon-reload'], check=True)
subprocess.run(['systemctl', 'enable', '--now', 'sharedata-forward'], check=True)
print('Base sharedata_dev, rol sharedata_app y secreto independiente preparados.')
