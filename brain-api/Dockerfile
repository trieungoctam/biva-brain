# Build context: gốc repo (cần cả brain-api/, contracts/ và kb/).
FROM golang:1.25 AS build
WORKDIR /src/brain-api
COPY brain-api/go.mod brain-api/go.sum ./
RUN go mod download
COPY brain-api/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/brain-api ./cmd/brain-api

FROM alpine:3.20
RUN adduser -D -u 10001 app && apk add --no-cache wget
COPY --from=build /out/brain-api /usr/local/bin/brain-api
COPY contracts /contracts
COPY kb /kb
ENV BIVA_MIGRATIONS_DIR=/contracts/migrations \
    BIVA_SCHEMAS_DIR=/contracts/schemas \
    BIVA_KB_DIR=/kb \
    BIVA_HTTP_ADDR=:8080
USER app
EXPOSE 8080
ENTRYPOINT ["brain-api"]
CMD ["serve"]
