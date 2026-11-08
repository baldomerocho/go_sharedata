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
sql = fr"""
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
# Entrada estable al Service (no un port-forward ligado a un pod).
service = {'apiVersion':'v1','kind':'Service','metadata':{'name':'sharedata','namespace':'sharedata'},'spec':{'selector':{'app':'sharedata'},'ports':[{'port':8844,'targetPort':8844}]}}
subprocess.run(['k3s','kubectl','apply','-f','-'],input=json.dumps(service),text=True,check=True)
print('Base sharedata_dev, rol sharedata_app y secreto independiente preparados.')
