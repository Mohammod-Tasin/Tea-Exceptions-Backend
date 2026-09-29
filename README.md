# teamachine

Minimal Go backend that unlocks a coffee machine. A website calls
`POST /api/unlock`; an ESP32 long-polls `GET /api/device/poll` to receive
the command.

Stdlib only, single in-memory machine, no persistence.

## Env vars

- `PORT` (default `8080`)
- `UNLOCK_SECONDS` (default `25`) - how long the machine stays busy after a command
- `FRONTEND_ORIGIN` (default `*`) - CORS allow-origin

## 1. Run locally without Docker

```
go run .
```

Test endpoints:

```
curl http://localhost:8080/healthz

curl http://localhost:8080/api/status

curl -X POST http://localhost:8080/api/unlock -d '{"drink":"coffee"}'
# -> 409 {"error":"machine offline"} until a device poll has been seen in the last 60s

# in another terminal, simulate the ESP32:
curl "http://localhost:8080/api/device/poll?wait=25"
# blocks until a command arrives or `wait` seconds elapse (then 204)

# now unlock will succeed and the poll above returns immediately:
curl -X POST http://localhost:8080/api/unlock -d '{"drink":"tea"}'

curl http://localhost:8080/api/status
```

## 2. Run locally with Docker

```
docker compose up --build
```

Same curl tests as above, against `http://localhost:8080`.

Watch logs:

```
docker compose logs -f
```

## 3. Deploy on Render

1. Create a new Web Service, connect this repo.
2. Set "Root Directory" to this folder. Render auto-detects the Dockerfile
   (runtime: Docker) - no build or start command needed.
3. Set env vars: `UNLOCK_SECONDS`, `FRONTEND_ORIGIN`.
4. Health check path: `/healthz`.

`PORT` is supplied by Render automatically - do not set it manually.

## 4. Later: VPS deploy (reference only)

Same Dockerfile:

```
docker compose up -d --build
```

Put a reverse proxy (Caddy or nginx) in front for TLS.
