# syntax=docker/dockerfile:1

# Comments are provided throughout this file to help you get started.
# If you need more help, visit the Dockerfile reference guide at
# https://docs.docker.com/go/dockerfile-reference/

# Want to help us make this template better? Share your feedback here: https://forms.gle/ybq9Krt8jtBL3iCk7

################################################################################
# Create a stage for building the application.
ARG GO_VERSION=1.25.5
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src

# Download dependencies as a separate step to take advantage of Docker's caching.
# Leverage a cache mount to /go/pkg/mod/ to speed up subsequent builds.
# Leverage bind mounts to go.sum and go.mod to avoid having to copy them into
# the container.
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

# This is the architecture you're building for, which is passed in by the builder.
# Placing it here allows the previous steps to be cached across architectures.
ARG TARGETARCH

# Build the application.
# Leverage a cache mount to /go/pkg/mod/ to speed up subsequent builds.
# Leverage a bind mount to the current directory to avoid having to copy the
# source code into the container.
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -o /bin/server .

################################################################################
# Create a stage for runtime assets we still want even in a scratch image.
# We use Alpine only to obtain CA certificates and to generate minimal user files,
# then copy just those files into the final scratch stage.
FROM alpine:3.20 AS runtime-assets

# Install CA certs for outbound TLS (HTTPS, WSS, etc.).
RUN apk --no-cache add ca-certificates

# Create a non-privileged user (copied into scratch via /etc/passwd and /etc/group).
ARG UID=10001
RUN addgroup -g "${UID}" appuser \
    && adduser -D -H -u "${UID}" -G appuser appuser

################################################################################
# Create a new stage for running the application that contains the minimal
# runtime dependencies for the application. This often uses a different base
# image from the build stage where the necessary files are copied from the build
# stage.
#
# The example below uses the alpine image as the foundation for running the app.
# By specifying the "latest" tag, it will also use whatever happens to be the
# most recent version of that image when you build your Dockerfile. If
# reproducibility is important, consider using a versioned tag
# (e.g., alpine:3.17.2) or SHA (e.g., alpine@sha256:c41ab5c992deb4fe7e5da09f67a8804a46bd0592bfdf0b1847dde0e0889d2bff).
FROM scratch AS final

# Copy runtime assets into the scratch image.
COPY --from=runtime-assets /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=runtime-assets /etc/passwd /etc/passwd
COPY --from=runtime-assets /etc/group /etc/group

# Copy the executable from the "build" stage.
COPY --from=build /bin/server /bin/

# Run as non-root.
ARG UID=10001
USER ${UID}:${UID}

# Expose the port that the application listens on.
EXPOSE 3334

# What the container should run when it is started.
ENTRYPOINT [ "/bin/server" ]