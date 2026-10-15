FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /app .

FROM alpine:3.20
RUN apk add --no-cache su-exec && adduser -D -u 10001 app && mkdir /data
COPY --from=build /app /app
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENV PORT=8080 DB_PATH=/data/app.db COOKIE_SECURE=true TRUST_PROXY=true
EXPOSE 8080
ENTRYPOINT ["/entrypoint.sh"]
