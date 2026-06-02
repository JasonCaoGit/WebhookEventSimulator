# TypeScript GitHub Webhook Simulator

A small local/dev app that simulates GitHub sending a pull request webhook to your Cloud Run ingestion service, then receives callback results from that service on `/comment`.

## Stack

- Backend: Express + TypeScript
- Frontend: React + Vite + TypeScript
- Callback updates: frontend polling every 1 second

## 1. Install dependencies

```bash
npm install
```

## 2. Configure environment

Copy the example file:

```bash
cp .env.example .env
```

Set these values in `.env`:

```bash
WEBHOOK_TARGET_URL=https://my-cloud-run-ingestion-url/github/webhook
PUBLIC_CALLBACK_URL=https://abc123.ngrok-free.app/comment
```

Optional:

```bash
WEBHOOK_SECRET=replace-me
PORT=80
```

When `WEBHOOK_SECRET` is set, the simulator signs the raw JSON body with HMAC-SHA256 and sends:

```text
X-Hub-Signature-256: sha256=<hex digest>
```

`PORT` is used by the built Express server. Use `PORT=80` when you want the built app at `http://localhost:80`.

## 3. Run locally

For local development:

```bash
npm run dev
```

Open:

```text
http://localhost:3000
```

The Vite dev server proxies `/api/*` and `/comment` to the Express backend on `http://localhost:4000`.

To run the built full-stack app on `http://localhost:80`:

```bash
npm run build
npm start
```

If port `80` is blocked on your machine, set another `PORT` in `.env`, for example `PORT=3000`.

## 4. Expose the simulator with ngrok

In dev mode, expose the Vite frontend port:

```bash
ngrok http 3000
```

ngrok will print a public URL like:

```text
https://abc123.ngrok-free.app
```

Set `PUBLIC_CALLBACK_URL` in `.env` to that URL plus `/comment`:

```bash
PUBLIC_CALLBACK_URL=https://abc123.ngrok-free.app/comment
```

Restart `npm run dev` after changing `.env`.

## 5. Set the Cloud Run webhook target

Set `WEBHOOK_TARGET_URL` to your Cloud Run ingestion endpoint:

```bash
WEBHOOK_TARGET_URL=https://your-cloud-run-url/github/webhook
```

## 6. Verify the flow

1. Open `http://localhost:3000`.
2. Confirm the UI shows the configured target webhook URL.
3. Click **Send Fake GitHub Event**.
4. If your Cloud Run service returns `2xx` within 10 seconds, the frontend shows:

```text
Webhook accepted: <id>
```

5. If the target fails or times out, the frontend shows:

```text
Webhook failed or timed out: <id>
```

6. When your service later sends this callback:

```json
{
  "id": "event-id",
  "message": "hello world"
}
```

to:

```text
POST /comment
```

the frontend shows:

```text
Received callback: hello world for id <id>
```

## API endpoints

### `POST /api/send-webhook`

Generates a UUID delivery id, builds a fake GitHub pull request payload, sends it to `WEBHOOK_TARGET_URL`, and waits up to 10 seconds for a `2xx` response.

Response:

```json
{
  "id": "...",
  "status": "accepted",
  "statusCode": 200
}
```

or:

```json
{
  "id": "...",
  "status": "failed",
  "statusCode": null,
  "error": "..."
}
```

### `POST /comment`

Accepts callback JSON and stores it in memory:

```json
{
  "id": "event-id",
  "message": "hello world"
}
```

Returns `200 OK`.

### `GET /api/comments`

Returns all callback messages currently stored in memory.
# WebhookEventSimulator
