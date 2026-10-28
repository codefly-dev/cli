# Package the existing CLI for infrastructure-owned publisher Jobs.
# The operator builds codefly from the exact source revision before building this image.
FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --chmod=0555 codefly /codefly
ENTRYPOINT ["/codefly"]
