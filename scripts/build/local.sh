echo "Installing locally"

if [ ! -d "build" ]; then
  mkdir build
fi
go build -o build/codefly main.go && mv build/codefly "$GOBIN/"
