# go Docker to build codefly and run flows

FROM golang:alpine

WORKDIR /home
COPY go.mod go.sum ./
RUN go mod download
COPY scripts ./scripts
COPY main.go ./
COPY cmd ./cmd
COPY generated ./generated
COPY pkg ./pkg
RUN /home/scripts/build/local.sh

# Create a group and user
RUN addgroup -S appgroup && adduser -S appuser -G appgroup
USER appuser

WORKDIR /home/appuser
