FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

ARG JQ_VERSION=1.6

COPY usr/bin/oc /usr/bin/oc

COPY oc_sa_create_kubeconfig.sh /usr/bin/oc_sa_create_kubeconfig.sh
COPY oc_create_kubeconfig.sh /usr/bin/oc_create_kubeconfig.sh
COPY generate_thanos_tls.sh /usr/bin/generate_thanos_tls.sh
COPY generate_thanos_client_tls.sh /usr/bin/generate_thanos_client_tls.sh

RUN microdnf install -y jq openssl

ADD ci-secret-generator /usr/bin/ci-secret-generator

ENTRYPOINT ["/usr/bin/ci-secret-generator"]
