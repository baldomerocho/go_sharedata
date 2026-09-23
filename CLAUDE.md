# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Qué es

ShareData: chat multicanal con compartición de archivos, cifrado extremo a extremo en el cliente, autenticación por OTP vía SMS + JWT. Todo el backend es **un solo archivo** (`main.go`, ~810 líneas) y todo el frontend es **un solo archivo** (`static/index.html`, ~3450 líneas: HTML + CSS + JS vanilla, sin build step ni dependencias npm). No hay tests.

## Comandos

```bash
go build -o sharedata .          # compilar
go run .                         # ejecutar (requiere PostgreSQL accesible como host "postgres-db")
go vet ./...                     # análisis estático
docker compose up -d --build     # producción (TLS=false, detrás de proxy)
docker compose logs -f sharedata
docker compose -p sharedata-dev -f docker-compose.dev.yml up -d --build   # entorno dev (puerto 8846)
```

El frontend se sirve vía `go:embed static/*`: **cualquier cambio en `static/index.html` exige recompilar el binario** (o reconstruir la imagen), no basta con recargar el navegador.

**El entorno dev usa la MISMA base de datos y el mismo `JWT_SECRET` que producción**: un borrado masivo desde dev destruye datos reales y los tokens valen en ambos. El `-p sharedata-dev` es obligatorio para que compose no trate el contenedor de producción como huérfano.

Las redes `databases_default` y `web-network` de ambos compose son externas y deben existir previamente.

`API2.md` documenta el contrato **vigente** para clientes externos (REST, protocolo WebSocket, parámetros exactos del E2EE y blurhash). Manténlo sincronizado al tocar handlers, tipos de evento o el esquema de cifrado. `API.md` es la v1 congelada: no la edites, describe el servidor anterior a la paginación y a los adjuntos bajo demanda.

## Arquitectura

### Flujo de datos

El transporte principal es WebSocket, no REST. Los mensajes **se envían por WS** (`{type:"new_message"}`), con el adjunto dentro; el servidor los persiste y hace broadcast a todos los clientes conectados (`broadcast()` en `main.go`). Los endpoints REST son para autenticación, historial, descarga de adjuntos, gestión de canales y borrados:

- `POST /api/auth/login` → genera OTP con `crypto/rand`, lo guarda en `otps` y lo envía al gateway `OTP_URL`. Enfriamiento de 60 s por teléfono (429 + `Retry-After`, serializado con `pg_advisory_xact_lock`); si el envío falla se borra el código, y si sale bien se anulan los anteriores
- `POST /api/auth/verify` → valida OTP con un único `UPDATE … RETURNING` atómico que consume un intento (máx. `otpMaxTries` = 5) y devuelve JWT (HS256 firmado a mano, sin librería, TTL 30 días)
- `/ws?token=…` → JWT por query string (los navegadores no permiten cabeceras en WebSocket)
- `authMW` acepta el token vía `Authorization: Bearer` o `?token=`, e inyecta `X-Phone` en la request

El broadcast es global: **todos los clientes reciben todos los eventos de todos los canales**; el filtrado por canal ocurre en el cliente.

### Adjuntos bajo demanda — no reintroducir `file_data` en el tráfico masivo

Los archivos viajan como data URLs base64 cifrados en la columna `file_data` de `messages` (no hay almacenamiento de blobs aparte de PostgreSQL). Para que la app no colapse, **ese campo nunca va en el tráfico masivo**:

- `GET /api/messages` devuelve solo metadatos y pagina hacia atrás por cursor (`before` = id del mensaje más antiguo que se tiene, `has_more`; `limit` 40 por defecto, 200 máx.). La primera página son los mensajes **más recientes**, invertidos a orden ascendente antes de responder. En el cliente, `loadOlder()` pide las páginas anteriores.
- El eco `new_message` del WS se retransmite con `FileData` vaciado.
- El contenido se pide con `GET /api/files/{id}` solo al abrir el adjunto. En el cliente, `ensureFile()` / `withFile()` descargan, descifran y cachean por id en `fileCache` (con `fileInflight` para no duplicar peticiones).

Un mensaje lleva un solo adjunto; el envío multi-archivo (hasta `MAX_FILES` = 10, `MAX_FILE_BYTES` = 20 MB cada uno) manda un mensaje por archivo, con el texto solo en el primero. El WS tiene `SetReadLimit` de 50 MB.

**Blurhash**: miniatura difuminada (4×3 componentes, cifrada como los demás campos) que la genera **el cliente que sube el archivo** (`makeBlurhash()`) porque el servidor no tiene la clave. Se pinta de fondo en la tarjeta del adjunto mientras no se ha descargado. Su columna se añade con `ALTER TABLE … ADD COLUMN IF NOT EXISTS` en `migrate()`.

### E2EE — la restricción de diseño central

El servidor nunca ve texto plano. En el navegador, `deriveKey()` deriva AES-256-GCM con PBKDF2 (SHA-256, 200 000 iteraciones, sal **fija y hardcodeada** `sharedata/v1/ws-salt`) a partir de una passphrase que el usuario introduce tras el OTP. Los campos `content`, `file_name`, `file_data` y `blurhash` se cifran con `encField()` antes de enviarse y se descifran con `decField()` al renderizar. El formato en base de datos es `E1:<iv_b64url>:<ciphertext_b64url>`.

Consecuencias que restringen cualquier cambio:

- La passphrase no viaja al servidor ni se persiste; vive en `sessionStorage` (`sd_pass`) y se re-deriva en cada arranque. No hay recuperación posible.
- El servidor **no puede** buscar, indexar, filtrar ni previsualizar contenido. Cualquier funcionalidad de ese tipo debe implementarse en el cliente.
- Si se cambia el esquema de cifrado hay que versionar `ENC_VER`; `decField()` devuelve el string tal cual si no empieza por el prefijo esperado, y `🔒 cifrado` si no hay clave.

### Base de datos

Todas las tablas (`users`, `otps`, `channels`, `messages`) se crean con `CREATE TABLE IF NOT EXISTS` en `migrate()` al arrancar; en una base vacía se siembra el canal 1 ("General"). El canal `id=1` es especial y no se puede eliminar. Borrar un canal elimina sus mensajes (y con ellos los adjuntos) con un `DELETE` explícito en la misma transacción, sin depender del `ON DELETE CASCADE` del esquema, que las bases antiguas pueden no tener. Los usuarios no se autoregistran: hay que insertarlos a mano (`INSERT INTO users (phone, name) VALUES (...)`), el login rechaza teléfonos desconocidos con 403.

Tras cada borrado masivo o de canal se lanza `VACUUM messages` en una goroutine, porque los data URLs base64 inflan la tabla.

### Frontend

Sin framework. Estado global en variables sueltas (`ws`, `chId`, `chs`, `msgCache`, `pending`, `fileCache`). Puntos a conocer antes de tocarlo:

- **Filas fantasma**: `addPendingRow()` pinta el mensaje optimistamente y `consumePendingFor()` lo reconcilia cuando llega el eco del broadcast, casando por `(username, hasFile, fileSize)` — no hay id de correlación, así que dos envíos idénticos simultáneos pueden confundirse.
- **Temas**: variables CSS en `:root` (oscuro por defecto) y `[data-theme="light"]`. El tema se aplica en un script inline en el `<head>` para evitar parpadeo. Nunca hardcodear colores; usar los tokens (`--bg`, `--surface`, `--text2`, `--gold`, …).
- **Previews**: miniaturas de PDF con pdf.js cargado bajo demanda desde CDN (`ensurePdfJs()`), carátulas de audio parseando tags ID3 a mano (`extractID3Cover()`), reproductores nativos y lightbox de imágenes. Como el historial no trae el archivo, las previews se generan a partir del data URL descargado y descifrado.
- **Menú contextual** por tipo de archivo (`buildCtxItems()`), con pulsación larga en táctil. Las acciones que necesitan el archivo pasan por `withFile()`.
- El nombre visible del usuario es independiente del teléfono: se asigna un nombre aleatorio de científicos/tecnólogos en el primer arranque y se guarda en `localStorage` (`sd_user`). El canal activo se guarda en `sd_ch` y el tema en `sd_theme`.

### Configuración

Casi todo está hardcodeado como constantes en la cabecera de `main.go` (credenciales de PostgreSQL, puerto `8844`, TTL y límites del OTP). Del entorno se leen `JWT_SECRET`, `TLS` y el gateway del OTP: `OTP_URL` (por defecto `http://otp-show-app-1:5066/api/otps`), `OTP_API_KEY` (cabecera `x-api-key`, se omite si está vacía) y `OTP_CHANNEL` (por defecto `"1"`). En Docker estas tres vienen de un `env_file` opcional en la raíz: `.env` en producción y `.env.develop` en dev (plantilla en `.env.example`; ninguno se versiona). El gateway sigue el mismo contrato que `GATEWAY_URL`/`X_API_KEY` de `go_otp_verify` (`POST {number, body, channel}`, éxito solo con 200), así que acepta tanto otp-show (sandbox, muestra el código en su PWA sin enviar SMS) como el webhook de SMS de producción. Con `TLS=true` (valor por defecto) el servidor genera un certificado autofirmado ECDSA en memoria al arrancar, con IPs fijas en `generateSelfSignedCert()` — ajústalas si cambia la red local. En Docker se usa `TLS=false` porque hay un proxy delante.
