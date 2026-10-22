# teamachine

Minimal Go backend that unlocks a coffee machine. A website calls
`POST /api/unlock`; an ESP32 long-polls `GET /api/device/poll` to receive
the command.

Stdlib only, single in-memory machine, no persistence.

## Env vars

- `PORT` (default `8080`)
- `UNLOCK_SECONDS` (default `25`) - how long the machine stays busy after a command
- `FRONTEND_ORIGIN` (default `*`) - CORS allow-origin
- `DEVICE_KEY` (default empty) - if set, ESP32 must send header `X-Device-Key` on /api/device/*.
- `STATUS_STALE_SECONDS` (default `30`) - device status older than this makes `machineState` `unknown`
- `TEMP_TABLE` (default `4.10:78,4.50:88,4.53:89`) - `volts:celsius` pairs, linearly interpolated for `tempC`

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

# device reports early consumption (ends the busy window immediately):
curl -X POST http://localhost:8080/api/device/consumed -d '{"id":"1","drink":"coffee"}'

# device reports heating state (204); unlock returns 409 not_ready unless "ready"
curl -X POST http://localhost:8080/api/device/status -d '{"thsVolts":4.532,"state":"ready"}'

# device refuses a command it received (204), clears it like consumed
curl -X POST http://localhost:8080/api/device/rejected -d '{"id":1,"drink":"tea","reason":"not_ready"}'
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

## 3. Deploy (VPS, automatic)

Every push to `main` triggers `.github/workflows/deploy.yml`, which SSHes into the VPS with a restricted key that can only run `/opt/teamachine/deploy.sh backend` (git reset to origin/main + `docker compose up -d --build tea-api`). Live at https://api.cafe.zerotouch.com.co. Required repo secrets: VPS_HOST, VPS_USER, VPS_SSH_KEY, VPS_KNOWN_HOSTS. Never edit code directly on the VPS.
