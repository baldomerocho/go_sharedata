# ShareData en el VPS de desarrollo

URL: https://sd.server.gt. Namespace k3s: `sharedata`.

## Dependencias

- PostgreSQL 17 de `dev-databases`; base **sharedata_dev**, propietario
  **sharedata_app**, sin privilegios de administración del motor.
- `JWT_SECRET` exclusivo y aleatorio. `TLS=false` detrás de Cloudflare.
- Gateway OTP con contrato `POST {number, body, channel}`, cabecera `x-api-key`
  opcional y respuesta 200. Configurar `OTP_URL`, `OTP_API_KEY`, `OTP_CHANNEL`.
- Insertar teléfonos autorizados en `users` mediante SQL parametrizado.
- Los adjuntos se guardan cifrados en PostgreSQL; no hace falta S3.
- El usuario elige su passphrase en el navegador. No hay recuperación de ella.

## CI/CD

El workflow `Build ShareData` prueba con PostgreSQL real, race y vet, compila en
un runner hospedado, publica la imagen por SHA y conserva el artefacto 7 días.
El job `Desplegar en k3s` usa el runner del VPS, sin checkout ni scripts del repo.
Solo push/workflow_dispatch de master (y feat/deploy-vps durante la puesta en marcha)
pueden desplegar; los PR no usan el runner del servidor.

El operador root-owned `/usr/local/sbin/sharedata-release` recibe la autorización
temporal de Actions por stdin, valida repo/run/rama/SHA y descarga el artefacto de
ese run directamente desde GitHub. Valida el digest, importa la imagen y actualiza
solo ShareData. Comprueba rollout, readiness interna y pública, incluida la cabecera
`X-ShareData-Revision`. Si falla, restaura el spec anterior y espera su recuperación.
Los despliegues se serializan; un push nuevo no cancela uno que ya esté desplegando.

El origen Cloudflare `localhost:8844` usa un proxy TCP al ClusterIP del Service,
no un port-forward ligado a un pod. `sharedata-forward.service` lo mantiene activo.
`deploy/prepare.py` conserva secretos y configura base, usuario, secreto y origen;
antes de ejecutarlo instalar `proxy.py` en `/usr/local/lib/sharedata/proxy.py`.
Para cambiar el operador, instalar `release.py` como root; el job no lo sobrescribe.

Configuración de confianza: `/opt/sharedata/deploy-settings.json`. Sudo permite
únicamente invocar el operador; no ejecutar kubectl ni scripts arbitrarios como root.
La inscripción GitHub del runner tiene el alcance del repo actual. Compartir una
inscripción entre productos requiere que sus repos pertenezcan a la misma organización,
o un flujo central explícito entre cuentas; no transferir repos automáticamente.

## Operación

Secretos protegidos: `/opt/sharedata/credentials.json`, permiso 0600. Nunca
versionarlos. Para cambiar OTP, actualizar ese archivo y reaplicar prepare.py,
luego reiniciar el Deployment para cargar el nuevo entorno.

Dos réplicas y RollingUpdate con maxUnavailable=0/maxSurge=1. La readiness incluye
la distribución compartida de eventos. SIGTERM retira readiness, espera la
propagación y cierra WS con 1012 antes del shutdown HTTP. El cliente reconecta y
recarga canales/mensajes. Los eventos se guardan en PostgreSQL y se despiertan con
LISTEN/NOTIFY; el contenido grande no viaja dentro de NOTIFY, solo el ID. Se borra
el histórico técnico de eventos después de un día; los mensajes siguen su propio
ciclo. Las migraciones se serializan y son compatibles con la revisión anterior.

La disponibilidad HTTP durante el despliegue y la reconexión de WS son garantías
distintas. Un único VPS/DB sigue siendo un punto de fallo de infraestructura.

La base persiste en el volumen independiente del PostgreSQL compartido. No hay
backup específico de ShareData todavía: programarlo antes de guardar datos que
se necesiten conservar. Vigilar tamaño de mensajes y adjuntos, y disco libre.

Validación de réplicas con PG descartable:
`TEST_DATABASE_URL=... go test -race ./...`. Incluye eventos mayores de 8 KiB,
publicaciones concurrentes y recepción exacta en ambas instancias.
