FROM gcr.io/distroless/static:latest
LABEL maintainers="Kubernetes Authors"
LABEL description="CSI External Attacher"

ARG ARCH

COPY ./bin/csi-attacher-${ARCH} csi-attacher
ENTRYPOINT ["/csi-attacher"]
