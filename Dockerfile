FROM golang:1-alpine AS builder

COPY . /build
WORKDIR /build
RUN CGO_ENABLED=0 go build -o /usr/bin/mautrix-syncproxy

FROM scratch

COPY --from=builder /usr/bin/mautrix-syncproxy /usr/bin/mautrix-syncproxy

ENV LISTEN_ADDRESS=:29332

CMD ["/usr/bin/mautrix-syncproxy", "-config", "env"]
