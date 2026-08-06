# ShareData — API para clientes (móvil / terceros)

Documento de referencia para implementar un cliente nativo. Todo lo descrito está en `main.go`; la implementación de referencia del cliente es `static/index.html`.

**Lo más importante antes de empezar:** el servidor almacena y retransmite datos cifrados y no tiene la clave. Un cliente que ignore la sección [E2EE](#e2ee-obligatorio) recibirá cadenas como `E1:xk2…:9fA…` en lugar de texto, y todo lo que envíe será ilegible para el resto de clientes. El cifrado no es opcional ni es una capa añadida: es el formato de los datos.

---

## 1. Base y transporte

| | |
|---|---|
| Puerto | `8844` |
| Esquema | `https`/`wss` si `TLS=true` (por defecto), `http`/`ws` si `TLS=false` |
| Content-Type | `application/json` en todas las peticiones con cuerpo |

Con `TLS=true` el servidor genera un **certificado autofirmado ECDSA en memoria en cada arranque** (`generateSelfSignedCert()`), válido para `localhost`, `127.0.0.1`, `0.0.0.0` y `192.168.0.8`. Cambia en cada reinicio, así que el pinning por huella no sirve. Android e iOS rechazan ese certificado por defecto: en desarrollo hay que añadir una excepción explícita (`network_security_config.xml` en Android, `NSAppTransportSecurity` en iOS). En producción el despliegue Docker usa `TLS=false` detrás de un proxy que termina TLS — apunta ahí y el problema desaparece.

El transporte principal es **WebSocket**. Los mensajes nuevos se envían por WS, no por REST; los endpoints REST sirven para autenticación, carga inicial del historial, gestión de canales y borrados.

---

## 2. Autenticación

Flujo de tres pasos: teléfono → OTP por SMS → passphrase de cifrado (esta última es puramente local, nunca toca el servidor).

**No hay autoregistro.** El teléfono debe existir en la tabla `users` antes del primer login; si no, `/api/auth/login` responde `403`. Los usuarios se dan de alta a mano en la base de datos.

### `POST /api/auth/login`

```json
{ "phone": "+593987654321" }
```

Genera un código de 6 dígitos (TTL 5 min), lo guarda y lo envía por SMS a través de un microservicio interno.

| Código | Cuerpo | Significado |
|---|---|---|
| `200` | `{"ok":true,"ttl":300}` | Código enviado; `ttl` en segundos |
| `400` | texto plano | Teléfono vacío o JSON inválido |
| `403` | `Número no registrado` | El teléfono no está en `users` |
| `502` | `No se pudo enviar el código` | El servicio de SMS falló |

No existe endpoint de reenvío: para reenviar, repite esta llamada. Cada llamada crea un OTP nuevo y **los anteriores siguen siendo válidos** hasta expirar (la verificación toma el más reciente que coincida). Tampoco hay rate limiting: si tu app permite pulsar "reenviar" en bucle, se disparan tantos SMS como pulsaciones.

### `POST /api/auth/verify`

```json
{ "phone": "+593987654321", "code": "482913" }
```

| Código | Cuerpo |
|---|---|
| `200` | `{"token":"eyJ…","phone":"+593987654321","name":"Baldo"}` |
| `400` | `Datos inválidos` (el código debe tener exactamente 6 caracteres) |
| `401` | `Código inválido o expirado` |

El `token` es un **JWT HS256** con claims `{phone, iat, exp}` y TTL de **30 días**. Guárdalo de forma segura (Keychain / EncryptedSharedPreferences). El servidor solo valida firma y expiración: no comprueba que el usuario siga existiendo, y no hay revocación ni refresh — cuando expira, el usuario repite el flujo de OTP.

### Envío del token

Dos formas, ambas aceptadas por el middleware:

```
Authorization: Bearer <token>      ← usa esta en REST
?token=<token>                     ← obligatoria en WebSocket
```

El WebSocket exige la query string porque los navegadores no permiten cabeceras en el handshake. En un cliente nativo **sí** podrías mandar cabeceras, pero el servidor solo lee la query en `/ws`.

Cualquier endpoint bajo `/api/` (salvo `login` y `verify`) devuelve `401 Unauthorized` sin token válido.

### `GET /api/auth/me`

→ `{"phone":"+593987654321","name":"Baldo"}` — `name` puede ser `""`.

---

## 3. E2EE (obligatorio)

El cliente cifra tres campos antes de enviarlos y los descifra al recibirlos. El servidor los trata como cadenas opacas.

### Derivación de clave

```
KDF:         PBKDF2-HMAC-SHA256
passphrase:  introducida por el usuario (UTF-8, sin normalizar)
salt:        "sharedata/v1/ws-salt"   ← bytes ASCII literales, sal FIJA y compartida
iteraciones: 200000
salida:      clave AES-256 (32 bytes)
```

La sal es constante y global: todos los usuarios con la misma passphrase derivan la misma clave. Eso es lo que permite que se lean entre sí — es un secreto compartido de grupo, no una identidad por usuario. La passphrase nunca se envía al servidor y **no hay recuperación**: si se pierde, el historial es irrecuperable para todos.

En la app de referencia se guarda en `sessionStorage` y la clave se re-deriva en cada arranque. En móvil, guarda la passphrase en el almacén seguro del sistema si quieres persistir sesión; ten en cuenta que 200 000 iteraciones tardan cientos de milisegundos en gama baja, así que deriva una vez y cachea el objeto clave en memoria.

### Formato de campo cifrado

```
E1:<base64url(iv)>:<base64url(ciphertext‖tag)>
```

- `E1` — versión del esquema. Un campo que **no** empiece por `E1:` se considera texto plano heredado y se muestra tal cual.
- `iv` — 12 bytes aleatorios, **nuevos en cada campo**, nunca reutilizados.
- Cifrado AES-256-GCM; el **tag de autenticación de 16 bytes va concatenado al final del ciphertext**. Es lo que hace WebCrypto por defecto, pero no lo que hacen las APIs nativas: en Java/Kotlin usa `GCMParameterSpec(128, iv)` (que ya espera el tag al final), y en Swift/CryptoKit usa `AES.GCM.SealedBox(combined:)` teniendo en cuenta que `combined` incluye el IV al principio — tendrás que ensamblarlo tú a partir de las dos partes.
- Base64 **url-safe y sin padding**: alfabeto `-_`, sin `=` final. En Android, `Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP`.
- Sin AAD (additional authenticated data).

### Qué se cifra y qué no

| Campo | Cifrado |
|---|---|
| `content` | **Sí** |
| `file_name` | **Sí** |
| `file_data` | **Sí** |
| `username` | No — viaja en claro |
| `file_size`, `channel_id`, `id`, `created_at` | No |
| Nombres de canal | No |

Los metadatos van en claro por diseño: el servidor necesita `channel_id` para enrutar y `file_size` para el emparejamiento de mensajes optimistas. Un cuerpo vacío se envía como `""`, no se cifra.

### Adjuntos

`file_data` es un **data URL completo** (`data:image/png;base64,iVBORw0…`) que se cifra entero como una sola cadena. No hay almacenamiento de blobs: el archivo viaja dentro del mensaje y se guarda en una columna de PostgreSQL.

Cadena de inflado de tamaño, a tener en cuenta al validar:

```
archivo 20 MB → data URL base64 ≈ 27 MB → AES-GCM + base64url ≈ 36 MB → cabe en el límite de 50 MB del WS
```

El cliente de referencia rechaza archivos de más de **20 MB**; el servidor corta la conexión WS por encima de **50 MB** por trama (`SetReadLimit`). Aplica el límite de 20 MB en tu app: si lo superas, el WS se cierra sin mensaje de error útil. Y cuenta con tener el archivo entero en memoria tres veces (bytes, base64, cifrado) — en móvil eso importa.

---

## 4. WebSocket

```
GET /ws?token=<jwt>
```

`401` si el token falta o es inválido. Sin ping/pong ni heartbeat por parte del servidor: implementa reconexión con backoff en el cliente y detecta la caída tú mismo.

**El broadcast es global.** Cada cliente conectado recibe *todos* los eventos de *todos* los canales, independientemente del canal que esté viendo. El filtrado por `channel_id` es responsabilidad del cliente. Tenlo presente: en móvil vas a recibir (y descifrar) adjuntos de canales que el usuario no está mirando.

### Cliente → servidor

**`new_message`**

```json
{
  "type": "new_message",
  "message": {
    "channel_id": 3,
    "username": "Ada Lovelace",
    "content": "E1:aBcD…:eFgH…",
    "has_file": true,
    "file_name": "E1:iJkL…:mNoP…",
    "file_size": 184320,
    "file_data": "E1:qRsT…:uVwX…"
  }
}
```

Normalización y descartes silenciosos en el servidor:

- `content` vacío **y** `has_file` false → el mensaje se **descarta sin avisar**
- `username` vacío → `"Anónimo"`
- `channel_id` ausente o `0` → `1`
- JSON malformado o `type` desconocido → se ignora sin cerrar la conexión

No hay ACK ni id de correlación. La confirmación de que un mensaje se guardó es el evento `new_message` que te llega de vuelta por broadcast. El cliente de referencia pinta una fila optimista y la reconcilia casando `(username, has_file, file_size)` — heurística frágil: dos envíos idénticos del mismo usuario en paralelo pueden cruzarse. Si implementas envío optimista, considera usar el `created_at`/`id` del eco para desambiguar.

**`delete_messages`** — mismo efecto que el endpoint REST equivalente:

```json
{ "type": "delete_messages", "channel_id": 3, "period": "today" }
```

### Servidor → cliente

| `type` | Campos | Cuándo |
|---|---|---|
| `new_message` | `message` (objeto completo, con `id` y `created_at`) | Mensaje guardado |
| `message_removed` | `message.id`, `message.channel_id` | Mensaje individual borrado |
| `messages_deleted` | `channel_id`, `period` | Borrado masivo — recarga el historial de ese canal |
| `channel_created` | `channel` | Canal nuevo |
| `channel_updated` | `channel` (`id`, `name`; `created_at` viene vacío) | Canal renombrado |
| `channel_deleted` | `channel_id` | Canal eliminado |

`messages_deleted` no dice qué se borró, solo el periodo: la única respuesta correcta es volver a pedir `/api/messages`.

---

## 5. REST

Todos requieren `Authorization: Bearer <token>`.

### Canales

| Método | Ruta | Cuerpo | Respuesta |
|---|---|---|---|
| `GET` | `/api/channels` | — | `[{"id":1,"name":"General","created_at":"…"}]`, orden ascendente por fecha |
| `POST` | `/api/channels` | `{"name":"Proyecto X"}` | El canal creado |
| `PUT` | `/api/channels/{id}` | `{"name":"Nuevo"}` | `{"ok":true}` |
| `DELETE` | `/api/channels/{id}` | — | `{"ok":true}` |

El canal **`id=1` ("General") no se puede eliminar** → `400`. Al borrar un canal el servidor solo ejecuta `DELETE FROM channels` y después un `VACUUM messages`, lo que implica que la limpieza de mensajes depende de un `ON DELETE CASCADE` en el esquema (la tabla `messages` no se crea en las migraciones del código: verifícalo en tu base de datos). Los nombres de canal no se cifran y no hay control de acceso por canal: cualquier usuario autenticado ve, crea, renombra y borra cualquier canal.

### Mensajes

**`GET /api/messages?channel_id=1&limit=200`**

```json
{
  "messages": [
    {
      "id": 41,
      "channel_id": 1,
      "username": "Ada Lovelace",
      "content": "E1:aBcD…:eFgH…",
      "has_file": false,
      "file_name": "",
      "file_size": 0,
      "file_data": "",
      "created_at": "2026-08-06T14:22:01.123Z"
    }
  ],
  "total": 41
}
```

- `channel_id` por defecto `1`; `limit` por defecto `200`, máximo `500` (valores fuera de rango se ignoran y cae al defecto).
- `total` es el recuento real de la tabla para ese canal, útil para saber si te falta historial.
- **No hay paginación.** La consulta es `ORDER BY created_at ASC LIMIT n`, así que devuelve los mensajes **más antiguos**, no los más recientes. Con más de 500 mensajes en un canal, un cliente móvil que solo llame a este endpoint mostrará el principio de la conversación y nunca el final. No hay parámetro de offset ni cursor: para arreglarlo hay que tocar `handleMessages` en el servidor.
- Descifra `content`, `file_name` y `file_data` de cada mensaje antes de mostrarlos. Con adjuntos grandes esto es trabajo pesado: hazlo fuera del hilo principal y bajo demanda cuando puedas.

**`POST /api/messages/delete`**

```json
{ "channel_id": 1, "period": "today" }
```

`period` ∈ `today` · `week` · `month` · `year` · `all`. Los periodos se calculan en el servidor con `date_trunc` sobre la **zona horaria de PostgreSQL**, no la del dispositivo. Respuesta `{"deleted": 12}`, y se emite `messages_deleted` a todos los clientes. Un `period` no reconocido → `500`.

**`DELETE /api/messages/delete/{id}`** → `{"ok":true}`. Devuelve `ok` aunque el id no exista. Cualquier usuario autenticado puede borrar mensajes de cualquier otro.

---

## 6. Checklist de implementación

1. Login por teléfono → OTP → guardar JWT en el almacén seguro.
2. Pedir la passphrase y derivar la clave AES (PBKDF2, 200k, sal fija). Cachear la clave en memoria; no la re-derives en cada mensaje.
3. `GET /api/channels` y `GET /api/messages?channel_id=…` → descifrar los tres campos.
4. Abrir `/ws?token=…`, con reconexión y backoff propios.
5. Filtrar los eventos entrantes por `channel_id`: llega todo.
6. Al enviar: cifrar `content` / `file_name` / `file_data` con un IV nuevo cada uno, y emitir por WS.
7. Tratar el eco de `new_message` como confirmación; con `messages_deleted`, recargar.

### Lo que el servidor no da y tendrás que resolver en el cliente

Búsqueda, indexado o previsualización en servidor (imposible: no tiene la clave) · paginación del historial · ACK de envío · notificaciones push · presencia o "escribiendo…" · lectura/no leído · permisos por canal · rate limiting · revocación de tokens.
