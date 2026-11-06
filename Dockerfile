# ---- build stage ----
FROM golang:1.27-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS builder

# Set by the container_image workflow to the git tag (vX.Y.Z); surfaces as
# collector_version in every snapshot.
ARG VERSION=dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/inventory-agent .

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

WORKDIR /

COPY --from=builder /out/inventory-agent /inventory-agent

# No port needed; it runs once per CronJob invocation and exits 0.
ENTRYPOINT ["/inventory-agent"]
