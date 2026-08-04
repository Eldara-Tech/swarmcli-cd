# The UI is built from this commit's sources rather than taken from the build
# context: .dockerignore excludes web/dist precisely so that a developer's stale
# local build cannot become what the image serves.
FROM node:26-alpine AS ui
WORKDIR /src/web/ui
# The manifests on their own first, so that editing a component does not re-run
# the install.
COPY web/ui/package.json web/ui/package-lock.json ./
RUN npm ci
COPY web/ui/ ./
# Writes ../dist, which is /src/web/dist — the directory the Go build embeds.
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Before the build, because //go:embed compiles it into the binary. web/dist is
# not in the build context, so dropping this line fails the build with "pattern
# all:dist: no matching files found" rather than quietly shipping an image that
# serves the not-built page.
COPY --from=ui /src/web/dist ./web/dist

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
