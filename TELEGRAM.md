# Canal Telegram en Libredesk

Esta copia de libredesk añade un canal **Telegram** (bot de la Bot API) con dos modos de recepción:

- **Polling**: Libredesk hace *long polling* (`getUpdates`) contra Telegram. Funciona en local o detrás de un firewall; no necesita URL pública.
- **Webhook**: al guardar la bandeja, Libredesk registra `setWebhook` con un token secreto aleatorio; Telegram envía cada mensaje a `POST /webhooks/telegram/{id_bandeja}` y Libredesk comprueba la cabecera `X-Telegram-Bot-Api-Secret-Token`. La URL raíz de la app (Ajustes → General) tiene que ser **HTTPS pública**.

Qué hace:

- Cada chat privado con el bot crea (o reutiliza) un contacto y una conversación; los mensajes de texto, fotos, documentos, audio/notas de voz, vídeo, stickers, ubicación y contactos llegan a la bandeja (los archivos se descargan, hasta 20 MB).
- Las respuestas del agente se envían al chat como texto (HTML convertido a texto) y los adjuntos como foto o documento.
- Los mensajes repetidos (reintentos de Telegram o del polling) se descartan por su id.
- El token del bot y el secreto del webhook se guardan cifrados y se ocultan en la interfaz.
- Cambiar de polling a webhook (o al revés) solo requiere editar la bandeja y guardar: el polling borra el webhook antes de empezar.

Límites: solo chats privados (los grupos se ignoran); un agente no puede iniciar una conversación nueva por Telegram (los bots no pueden escribir primero); las encuestas CSAT no se envían por Telegram.

## Arrancar con Docker

```sh
docker compose -f docker-compose.build.yml up -d --build
```

- Compila frontend y backend desde este código e inicia Postgres y Redis.
- `config.toml` (clave de cifrado aleatoria) y `.env` (contraseña inicial del usuario `System`) ya están creados y no se suben a git.
- Abre http://localhost:9000 e inicia sesión con el usuario **System** y la contraseña de `LIBREDESK_SYSTEM_USER_PASSWORD` en `.env`.
- Admin → Bandejas → Nueva bandeja → **Telegram** → pega el token de @BotFather y elige el modo.

### Si Docker no tiene salida a internet (Kiri)

En este equipo los contenedores de Kiri no pueden descargar nada, así que la compilación dentro de Docker falla. Alternativa ya probada: compilar en el host y arrancar con la imagen alpine montando el binario.

```sh
cd frontend && npx pnpm@11.28.8 install --frozen-lockfile && npx pnpm@11.28.8 build:main && npx pnpm@11.28.8 build:widget && npx pnpm@11.28.8 build:widget-loader && cd ..
CGO_ENABLED=0 go build -ldflags="-s -w -X 'main.versionString=v2.10.0-rc.3-telegram' -X 'github.com/abhinavxd/libredesk/internal/version.Version=v2.10.0-rc.3-telegram'" -o bin/libredesk ./cmd/
go install github.com/knadh/stuffbin/...@v1.3.0
stuffbin -a stuff -in bin/libredesk -out bin/libredesk frontend/dist i18n schema.sql static/email-templates static/public static/widget.min.js:static/widget.js
docker compose -f docker-compose.prebuilt.yml up -d
```

Ojo: mientras Kiri bloquee la salida a internet, la app tampoco puede hablar con api.telegram.org, así que el bot no recibirá ni enviará mensajes hasta que Kiri permita la red.

Para el modo webhook en local puedes exponer el puerto 9000 con un túnel HTTPS (por ejemplo cloudflared o ngrok), poner esa URL como URL raíz en Ajustes y volver a guardar la bandeja.
