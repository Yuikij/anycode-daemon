# AnyCode Daemon (Go Edition) - Architecture & API Documentation

## Overview
`anycode-daemon` is a lightweight, edge-deployable background service written in Go. It acts as the bridge between the AnyCode client (iOS App / Web Interface) and the remote development environment. It provides real-time bidirectional communication via WebSockets.

## Core Features
1. **WebSocket RPC Server**: Listens on a specified port (default: 9527) and processes JSON-RPC 2.0 style requests.
2. **File System Management**: Browse directories, read files, and write/apply file diffs.
3. **Git Integration**: Get git status, diffs, logs, and commit history.
4. **Agent Integration**: Interfaces with AI coding agents (Codex, Gemini) providing a unified API for the client.
5. **Secure Authentication**: Requires a token-based authentication handshake immediately after connection.

## Communication Protocol
The Daemon uses a JSON-RPC-like message format over a single WebSocket connection. 

Shared protocol assets now live in `/Users/soukon/code/anycode/protocol`, including versioned JSON Schema and sample payloads for the AnyCode event envelope, `events.resume`, and the `client.hello` / `server.hello` session handshake.

### WebSocket Connection
- **Endpoint**: `ws://<host>:<port>/`
- **Default Port**: `9527`

### Message Structure (JSON-RPC)
Every message sent to the daemon must follow this structure:
```json
{
  "jsonrpc": "2.0",
  "id": "<request_id>",
  "method": "<method_name>",
  "params": {
    // ... parameters specific to the method
  }
}
```

### Authentication Flow
Immediately after establishing the WebSocket connection, the client **must** send an `auth` method request. Until authenticated, all other requests will be rejected with a `401 Unauthorized` error.

**Request:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "auth",
  "params": {
    "token": "<auth_token>"
  }
}
```
**Response (Success):**
```json
{
  "id": 1,
  "result": {
    "ok": true,
    "codexAvailable": true
  }
}
```

### Recovery Semantics
- Broadcast and replayed notifications carry the `AnyCode Event Envelope v1` metadata (`seq`, `agent`, `projectId`, `projectGeneration`, `operationId`, `ts`).
- `client.hello` returns protocol/session metadata and nests the current resume result so reconnect setup happens in one round trip after auth.
- `server.hello.capabilities` is the negotiated intersection of the client-requested session capabilities and the daemon-supported set, currently `client.hello` and `project.generation`.
- `project.generation` controls whether `server.hello.project`, nested resume project payloads, and replayed event envelopes include generation metadata; when it is not negotiated those fields are omitted from the `client.hello` response path.
- `events.resume` returns `cursorExpired: true` plus a `snapshot` when the requested cursor has fallen out of the retained journal window.
- Clients should treat `taskStatus` as a fallback when `client.hello` recovery cannot be applied; the primary recovery path is the `client.hello` handshake with nested resume metadata.

## API Methods Reference

### 1. File System (`fs.*`)
- **`fs.browse`**: Browse directories (absolute path). Parameters: `path` (string), `showHidden` (bool).
- **`fs.readAbsolute`**: Read an absolute file path. Parameters: `path` (string).
- **`fs.tree`**: Get file tree with depth. Parameters: `path` (string), `depth` (int).

### 2. Project Management (`project.*`)
- **`project.open`**: Switch the daemon's working directory. Parameters: `path` (string).
- **`project.list`**: List available project directories.

### 3. Git Operations (`git.*`)
- **`git.status`**: Get working directory git status.
- **`git.diff`**: Get diffs for specific files. Parameters: `path` (string), `cwd` (optional).
- **`git.log`**: Get commit history. Parameters: `count` (int).

### 4. Agent Operations (`codex.*` / `gemini.*`)
- **`codex.start` / `codex.stop`**: Manage the Codex agent process.
- **`codex.applyFileChanges` / `codex.revertFileChanges`**: Apply or revert unified diff patches. Parameters: `changes` (array).
- **`gemini.prompt`**: Send a prompt to the Gemini agent. Parameters: `sessionId` (string), `prompt` (string), `images` (array).

## Deployment & Execution
The daemon is compiled as a static Go binary (`anycode-daemon`). It is typically deployed as a systemd service (`anycode-daemon.service`) on the remote target machine.

**Start Command Example:**
```bash
/usr/local/bin/anycode-daemon --port 9527 --token <your_secure_token>
```

**Environment Setup (via SSH Tunnel in iOS App):**
The AnyCode iOS client establishes an SSH tunnel to securely expose the remote daemon's port (`9527`) to the localhost of the mobile device. The client then communicates with the daemon over `ws://127.0.0.1:<local_forwarded_port>`. For Web interfaces, the daemon must either be exposed publicly, routed through a reverse proxy (e.g., Nginx, Cloudflare Tunnel), or connected via a secure WebSocket Relay.
