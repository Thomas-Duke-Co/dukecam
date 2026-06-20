FROM golang:1.25-alpine AS builder

# CGO required for goheif (HEIC/HEIF decoding via libde265)
RUN apk add --no-cache git gcc g++ musl-dev libde265-dev
WORKDIR /build
COPY . .
RUN go mod download && \
    go build -ldflags="-s -w" -o dukecam .

FROM alpine:3.20
# libde265 runtime library required by goheif
RUN apk add --no-cache ca-certificates libde265
WORKDIR /app
COPY --from=builder /build/dukecam .
COPY --from=builder /build/templates ./templates
COPY --from=builder /build/static ./static
COPY --from=builder /build/comparison ./comparison

EXPOSE 4010
CMD ["./dukecam"]
