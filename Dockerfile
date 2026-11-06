# ---- build stage ----
FROM golang:1.26-alpine@sha256:87c65a688975c0b1d4585d204874c45ca78354670e3a5f11368c5a492303d696 AS builder

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
