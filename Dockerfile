FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bitchord-plus-companion .

FROM python:3.12-slim-bookworm AS taglib-build
RUN apt-get update && apt-get install -y --no-install-recommends g++ libtag1-dev nlohmann-json3-dev && rm -rf /var/lib/apt/lists/*
COPY ops/taglib.cpp /src/taglib.cpp
RUN g++ -std=c++17 -O2 -Wall -Wextra /src/taglib.cpp -ltag -o /glaze-taglib

FROM python:3.12-slim-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends libtag1v5 libchromaprint-tools && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir streamrip==2.1.0 && useradd -u 10001 -m companion && mkdir /state && chown companion:companion /state
COPY --from=build /out/bitchord-plus-companion /usr/local/bin/bitchord-plus-companion
COPY --from=taglib-build /glaze-taglib /usr/local/bin/glaze-taglib
COPY qobuz_config.py /usr/local/lib/glaze/qobuz_config.py
USER companion
EXPOSE 8080
ENTRYPOINT ["bitchord-plus-companion"]
