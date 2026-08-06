# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Qué es

ShareData: chat multicanal con compartición de archivos, cifrado extremo a extremo en el cliente, autenticación por OTP vía SMS + JWT. Todo el backend es **un solo archivo** (`main.go`, ~740 líneas) y todo el frontend es **un solo archivo** (`static/index.html`, ~2500 líneas: HTML + CSS + JS vanilla, sin build step ni dependencias npm). No hay tests.

## Comandos

```bash
go build -o sharedata .          # compilar
go run .                         # ejecutar (requiere PostgreSQL accesible como host "postgres-db")
go vet ./...                     # análisis estático
docker compose up -d --build     # despliegue real (TLS=false, detrás de proxy)
docker compose logs -f sharedata
```

El frontend se sirve vía `go:embed static/*`: **cualquier cambio en `static/index.html` exige recompilar el binario**, no basta con recargar el navegador.

Las redes `databases_default` y `web-network` de `docker-compose.yml` son externas y deben existir previamente.

`API2.md` documenta el contrato **vigente** para clientes externos (REST, protocolo WebSocket, parámetros exactos del E2EE y blurhash). Manténlo sincronizado al tocar handlers, tipos de evento o el esquema de cifrado. `API.md` es la v1 congelada: no la edites, describe el servidor anterior a la paginación y a los adjuntos bajo demanda.

## Arquitectura

### Flujo de datos

El transporte principal es WebSocket, no REST. Los mensajes **se envían por WS** (`{type:"new_message"}`) y el servidor los persiste y hace broadcast a todos los clientes conectados (`broadcast()` en `main.go`). Los endpoints REST son solo para lectura inicial, gestión de canales y borrados:

- `POST /api/auth/login` → genera OTP, lo guarda en `otps` y lo envía al microservicio externo `otp-show-app-1:5066`
- `POST /api/auth/verify` → valida OTP, devuelve JWT (HS256 firmado a mano, sin librería, TTL 30 días)
- `/ws?token=…` → JWT por query string (los navegadores no permiten cabeceras en WebSocket)
- `authMW` acepta el token vía `Authorization: Bearer` o `?token=`, e inyecta `X-Phone` en la request

El broadcast es global: **todos los clientes reciben todos los eventos de todos los canales**; el filtrado por canal ocurre en el cliente.

### E2EE — la restricción de diseño central

El servidor nunca ve texto plano. En el navegador, `deriveKey()` deriva AES-256-GCM con PBKDF2 (SHA-256, 200 000 iteraciones, sal **fija y hardcodeada** `sharedata/v1/ws-salt`) a partir de una passphrase que el usuario introduce tras el OTP. Los campos `content`, `file_name` y `file_data` se cifran con `encField()` antes de enviarse y se descifran con `decField()` al renderizar. El formato en base de datos es `E1:<iv_b64url>:<ciphertext_b64url>`.

Consecuencias que restringen cualquier cambio:

- La passphrase no viaja al servidor ni se persiste; vive en `sessionStorage` (`sd_pass`) y se re-deriva en cada arranque. No hay recuperación posible.
- El servidor **no puede** buscar, indexar, filtrar ni previsualizar contenido. Cualquier funcionalidad de ese tipo debe implementarse en el cliente.
- Si se cambia el esquema de cifrado hay que versionar `ENC_VER`; `decField()` devuelve el string tal cual si no empieza por el prefijo esperado, y `🔒 cifrado` si no hay clave.

Los archivos se transportan como data URLs base64 dentro del propio mensaje (columna `file_data`), límite de 20 MB en el cliente, `SetReadLimit` de 50 MB en el WS. No hay almacenamiento de blobs aparte de PostgreSQL.

### Base de datos

Las tablas `users` y `otps` se crean en `migrate()` al arrancar. Las tablas `channels` y `messages` **no** están en las migraciones: deben preexistir. El canal `id=1` ("General") es especial y no se puede eliminar. Los usuarios no se autoregistran: hay que insertarlos a mano (`INSERT INTO users (phone, name) VALUES (...)`), el login rechaza teléfonos desconocidos con 403.

Tras cada borrado masivo se lanza `VACUUM messages` en una goroutine, porque los data URLs base64 inflan la tabla.

### Frontend

Sin framework. Estado global en variables sueltas (`ws`, `chId`, `chs`, `msgCache`, `pending`). Puntos a conocer antes de tocarlo:

- **Filas fantasma**: `addPendingRow()` pinta el mensaje optimistamente y `consumePendingFor()` lo reconcilia cuando llega el eco del broadcast, casando por `(username, hasFile, fileSize)` — no hay id de correlación, así que dos envíos idénticos simultáneos pueden confundirse.
- **Temas**: variables CSS en `:root` (oscuro por defecto) y `[data-theme="light"]`. El tema se aplica en un script inline en el `<head>` para evitar parpadeo. Nunca hardcodear colores; usar los tokens (`--bg`, `--surface`, `--text2`, `--gold`, …).
- **Previews**: miniaturas de PDF con pdf.js cargado bajo demanda desde CDN (`ensurePdfJs()`), carátulas de audio parseando tags ID3 a mano (`extractID3Cover()`), reproductores nativos y lightbox de imágenes. Todo se genera desde el data URL ya descifrado, dentro de `requestIdleCallback`.
- **Menú contextual** por tipo de archivo (`buildCtxItems()`), con pulsación larga en táctil.
- El nombre visible del usuario es independiente del teléfono: se asigna un nombre aleatorio de científicos/tecnólogos en el primer arranque y se guarda en `localStorage` (`sd_user`).

### Configuración

Casi todo está hardcodeado como constantes en la cabecera de `main.go` (credenciales de PostgreSQL, host y canal del servicio OTP, puerto). Solo `JWT_SECRET` y `TLS` se leen del entorno. Con `TLS=true` (valor por defecto) el servidor genera un certificado autofirmado ECDSA en memoria al arrancar, con IPs fijas en `generateSelfSignedCert()` — ajústalas si cambia la red local. En Docker se usa `TLS=false` porque hay un proxy delante.
