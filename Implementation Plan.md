# Cloud Run Deployment Plan

This plan details how to deploy the Comic Chat Multiplayer application to Google Cloud Run while addressing statelessness (image storage) and WebSocket connection routing.

## Architectural Considerations

### 1. WebSockets & Scaling
*   **Problem**: Cloud Run instances scale dynamically. If two users connect to the same room but land on different server instances, they won't receive each's messages because the room state is in-memory.
*   **Solution**: 
    1.  **Hobby/Single Instance Configuration**: Set `max-instances=1` on Cloud Run. This is the simplest and cheapest approach, ensuring all connections land on the exact same instance, keeping WebSockets functional without complex Redis backends.
    2.  **Session Affinity**: Enable session affinity on Cloud Run, which routes requests from the same client to the same container instance. However, if the room has multiple users, they may still end up on different containers. For true horizontal scaling, a Redis Pub/Sub adapter would be needed. 
    *   **Recommendation**: We should start by deploying with `max-instances=1` as it fits hobby usage perfectly and requires zero architectural rewrites.

### 2. Image Storage (Statelessness)
*   **Problem**: Cloud Run filesystems are ephemeral. Any generated images saved to `static/generated/` will be deleted whenever the container scales down to 0 or restarts.
*   **Solution**:
    1.  **Ephemeral in-memory `/tmp`**: Change the storage path to `/tmp/generated` and serve it. This is fast but images disappear on container spin-down.
    2.  **Google Cloud Storage (GCS) Mount**: Mount a GCS Bucket directly to the Cloud Run container at `/app/static/generated` using the Cloud Run GCS volume mount feature. This keeps the Go code completely unchanged (it still writes to the filesystem), but GCS handles persistence!
    *   **Recommendation**: Use a GCS Bucket mounted to `/app/static/generated`.

---

## Proposed Changes

### Dockerfile [NEW]

We need a multi-stage Dockerfile to build and package our Go binary and the static assets.

#### [NEW] [Dockerfile](file:///Users/neilghosh/dev/comic-chat-multiplayer/Dockerfile)
```dockerfile
# Build Stage
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o comic-chat-server .

# Run Stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app
COPY --from=builder /app/comic-chat-server .
COPY --from=builder /app/static ./static

EXPOSE 8080
CMD ["./comic-chat-server"]
```

---

## Deployment Steps

This repo now includes a GitHub Actions workflow at `.github/workflows/deploy-cloud-run.yml` that performs build + deploy.
It is configured with:
- **Project ID**: `demoneil`
- **Region**: `us-central1`
- **Service**: `comic-chat`
- **Bucket**: `demoneil-comic-panels`

### GitHub Actions deployment (recommended)

1. Add repository secrets:
   - `GCP_WORKLOAD_IDENTITY_PROVIDER`
   - `GCP_SERVICE_ACCOUNT`
   - `GEMINI_API_KEY`
2. Trigger the workflow:
   - Push to `main`, or
   - Run **Deploy to Cloud Run** manually via **Actions → workflow_dispatch**
3. The workflow:
   - Builds/pushes container image to Artifact Registry
   - Ensures GCS bucket exists
   - Deploys Cloud Run with:
     - `max-instances=1`
     - `timeout=3600`
     - `GENERATED_DIR=/app/static/generated`
     - GCS mount at `/app/static/generated`

## Open Questions

> [!IMPORTANT]
> - The deployment is currently configured for single-instance Cloud Run (`max-instances=1`) to keep in-memory room state consistent.
> - If you want horizontal scaling later, we should add Redis Pub/Sub and remove the single-instance constraint.