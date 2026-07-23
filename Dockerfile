FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev

# Two versions are stamped and they are not the same thing. VERSION is this
# binary's own tag. The chart engine's version is the swarmcli release this
# module pins, read from go.mod rather than passed in so it cannot drift from
# what is actually compiled in — without it charts.EngineVersion() is empty and
# every chart declaring a swarmcliVersion floor is deployed unchecked.
RUN ENGINE=$(go list -m -f '{{.Version}}' github.com/Eldara-Tech/swarmcli) && \
    go build -trimpath -ldflags="-s -w \
      -X github.com/Eldara-Tech/swarmcli-cd/controller.version=${VERSION} \
      -X github.com/Eldara-Tech/swarmcli/charts.engineVersion=${ENGINE}" \
      -o /swarmcli-cd ./cmd/swarmcli-cd

FROM alpine:3.24
# ca-certificates only: the controller clones over HTTPS and pulls chart
# repository indexes, and an image without them fails at the first fetch with a
# certificate error that looks like a repository problem.
RUN apk add --no-cache ca-certificates
COPY --from=build /swarmcli-cd /swarmcli-cd

# No docker binary. The applier is built on the moby client, so the daemon is
# reached over the mounted socket rather than by shelling out — which is what
# CE's backend would have required.
LABEL org.opencontainers.image.source="https://github.com/Eldara-Tech/swarmcli-cd"
LABEL org.opencontainers.image.title="swarmcli-cd"
LABEL org.opencontainers.image.description="GitOps continuous delivery for Docker Swarm"

ENTRYPOINT ["/swarmcli-cd"]
CMD ["controller"]
