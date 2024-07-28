echo "Installing locally"

if [ -z "$GOPATH" ]
then
  echo "GOBIN is not set. Please set it before running this script."
  exit 2
fi

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-extldflags "-static"' -o ../core/bin/linux/codefly
