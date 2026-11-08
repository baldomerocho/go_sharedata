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

## Compilación y despliegue

GitHub Actions `Build ShareData` ejecuta pruebas, vet y compila una imagen Linux
amd64. Publica por SHA en GHCR y conserva un tar como artefacto durante 7 días.
El VPS importa esa imagen; no compila código ni imágenes.

1. Ejecutar `deploy/prepare.py` en el VPS. Conserva los secretos existentes,
   crea la base y rol si faltan, y registra el port-forward privado.
2. Descargar el artefacto del run correcto con `gh run download`, copiar al VPS
   e importar: `k3s ctr images import /ruta/sharedata.tar`.
3. Sustituir `SHAREDATA_IMAGE` de `deploy/k3s.yaml` por la imagen con su SHA y
   aplicar con `k3s kubectl apply -f ...`.
4. Esperar `kubectl -n sharedata rollout status deployment/sharedata` y comprobar
   `/readyz`, acceso público y autenticación REST/WS.
5. Retirar el tar transferido tras importarlo para no duplicar uso de disco.

El gateway Cloudflare existente ya dirige `sd.server.gt` a `http://localhost:8844`.
`sharedata-forward.service` publica solo ese localhost, con reinicio automático.
No se alteran rutas ni secretos de AppNodo. La NetworkPolicy adicional permite
únicamente que pods `app=sharedata` del namespace `sharedata` accedan a PostgreSQL;
los demás motores permanecen aislados.

## Operación

Secretos protegidos: `/opt/sharedata/credentials.json`, permiso 0600. Nunca
versionarlos. Para cambiar OTP, actualizar ese archivo y reaplicar prepare.py,
luego reiniciar el Deployment para cargar el nuevo entorno.

Una sola réplica: el broadcast WebSocket es local al proceso. Las actualizaciones
reinician el proceso y los clientes deben reconectar; no se garantiza continuidad
sin cortes. Antes de escalar hay que externalizar el broadcast.

La base persiste en el volumen independiente del PostgreSQL compartido. No hay
backup específico de ShareData todavía: programarlo antes de guardar datos que
se necesiten conservar. Vigilar tamaño de mensajes y adjuntos, y disco libre.

No incluye despliegue continuo automático: el workflow construye/publica y el
paso de despliegue en k3s se ejecuta explícitamente.
