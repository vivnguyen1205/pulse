
# Pulse
 
A lightweight observability platform built from scratch in Go. Agents collect system metrics from machines and send them to a central server, which stores them for querying and visualization.
 
## Architecture
 
```
[Agent (Go)] ──HTTP POST /metrics──> [Server (Go)] ──> [PostgreSQL]
```
 
- **Agent** (`agent/`): runs on each machine and collects CPU and memory usage every 5 seconds using [gopsutil](https://github.com/shirou/gopsutil). Sends metrics to the server as JSON.
- **Server** (`server/`): HTTP API that validates incoming metrics and writes them to PostgreSQL.
- **Database**: PostgreSQL running in Docker, with a `metrics` table indexed on `(host, name, ts)` for time-range queries.
## Getting started
 
**Requirements:** Go 1.22+, Docker
 
1. Start the database:
```bash
   docker compose up -d
```
 
2. Start the server:
```bash
   cd server
   go run .
```
 
3. Start the agent (in a new terminal):
```bash
   cd agent
   go run .
```
 
4. Verify data is arriving:
```bash
   docker compose exec db psql -U pulse -d pulse -c "SELECT * FROM metrics ORDER BY ts DESC LIMIT 10;"
```
 
### Configuration
 
| Variable | Used by | Default |
|---|---|---|
| `PULSE_SERVER` | agent | `http://localhost:8080` |
| `DATABASE_URL` | server | `postgres://pulse:pulse@localhost:5432/pulse` |
 
## API
 
### `POST /metrics`
 
Accepts a JSON array of metrics. Returns `202 Accepted` on success.
 
```json
[
  { "host": "my-laptop", "name": "cpu.percent", "value": 42.5, "timestamp": 1792952826 }
]
```
 
