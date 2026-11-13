# One image holds all four programs; docker-compose picks which one each
# container runs.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 1000 torza
COPY --from=build /out/ /usr/local/bin/
# Same user ID as qBittorrent (PUID=1000 in docker-compose), so both can
# read and move the downloaded files.
USER torza
