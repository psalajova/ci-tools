FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

RUN microdnf install -y git python3 python3-pip && \
      microdnf clean all && \
      pip install pyyaml

ADD ci-operator-checkconfig /usr/bin/ci-operator-checkconfig
ENTRYPOINT ["/usr/bin/ci-operator-checkconfig"]
