echo "Installing locally"

if [ -z "$GOPATH" ]
then
  echo "GOBIN is not set. Please set it before running this script."
  exit 2
fi

go build -o build/codefly main.go && mv build/codefly "$GOPATH/bin/"
