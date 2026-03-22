# Butler Provider Harvester Controller
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

# Subdirectory name when using parent context (e.g., butleradm --local)
ARG REPO_DIR=.

WORKDIR /workspace
RUN apk add --no-cache git make
COPY ${REPO_DIR}/go.mod ${REPO_DIR}/go.sum ./
RUN go mod download
COPY ${REPO_DIR}/ .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o manager cmd/main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /workspace/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
