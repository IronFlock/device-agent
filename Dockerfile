# Builder for `just build-all-docker`: cross-compiles the agent (with frpc
# built from source by the frp/ module) for every target in `targets`.
FROM golang:1.27.3-alpine

RUN apk add --no-cache bash git findutils build-base

WORKDIR /app/reagent

RUN mkdir -p src/ frp/ build/

COPY src/go.mod src/go.sum src/
COPY frp/go.mod frp/go.sum frp/

RUN cd src && go mod download && cd ../frp && go mod download

COPY src/ src/
COPY scripts/ scripts/
COPY targets targets

ENTRYPOINT [ "scripts/build-all.sh" ]
