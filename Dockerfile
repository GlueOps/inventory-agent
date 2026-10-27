# ---- build stage ----
FROM golang:1.26-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS builder

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
