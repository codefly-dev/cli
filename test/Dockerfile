# go Docker to build codefly and run flows

FROM golang:alpine AS builder

WORKDIR /home
COPY go.mod go.sum ./
RUN go mod download
COPY scripts ./scripts
COPY main.go ./
COPY cmd ./cmd
COPY pkg ./pkg
RUN /home/scripts/build/local.sh

# Second stage: Create the final Docker image
FROM alpine

# Create a group and user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

# Copy the binary from the first stage
COPY --from=builder /go/bin/codefly /bin/codefly

USER appuser
WORKDIR /codefly
