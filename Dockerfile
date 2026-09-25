FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/storage ./cmd/storage

FROM alpine:3.22
RUN apk add --no-cache ca-certificates su-exec \
	&& mkdir -p /data
COPY docker-entrypoint.sh /entrypoint.sh
COPY --from=build /out/storage /usr/local/bin/storage
VOLUME /data
EXPOSE 9000
ENV PORT=9000 \
	STORAGE_DATA=/data
ENTRYPOINT ["sh", "/entrypoint.sh"]
