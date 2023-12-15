# go Docker to build codefly and run flows
# Usage: docker build -t codefly-test .

FROM golang:alpine
WORKDIR /home
ADD go.mod go.sum ./
RUN go mod download
ADD . .
RUN ./scripts/build/install.sh
