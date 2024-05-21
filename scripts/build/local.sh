echo "Installing locally"

if [ -z "$GOPATH" ]
then
  echo "GOBIN is not set. Please set it before running this script."
  exit 2
fi
if [ ! -d "build" ]; then
  mkdir build
fi
go build -o build/codefly main.go && mv build/codefly "$GOPATH/bin/"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-extldflags "-static"' -o ../core/bin/codefly
