# go Docker to build codefly and run flows
# Usage: docker build -t codefly-test .

FROM golang:alpine
WORKDIR /home

COPY go.mod go.sum ./
RUN go mod download
COPY scripts ./scripts
COPY main.go ./
COPY cmd ./cmd
COPY generated ./generated
COPY pkg ./pkg
RUN /home/scripts/build/install.sh
RUN codefly init --with-default
COPY test/tracks ./tracks
RUN codefly replay --dir=tracks --track=plaform
