FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

ARG VERSION=dev
ARG REF=master

WORKDIR /app
RUN git clone https://github.com/mbartsch/dns-sync.git . && \
    git checkout ${REF}
RUN go mod download
RUN COMMIT=$(git rev-parse --short HEAD) && \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o dns-sync .

# ---
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/dns-sync /dns-sync

ENTRYPOINT ["/dns-sync"]
