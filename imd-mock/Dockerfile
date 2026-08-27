# Mock IMD weather API.
#
# No modules to download and CGO off, so the binary runs on any base and the
# build needs nothing from the network beyond the toolchain image.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/imd-mock .

FROM alpine:3.20
RUN adduser -D -u 10001 mock
USER mock
COPY --from=build /out/imd-mock /usr/local/bin/imd-mock
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/imd-mock"]
