FROM registry.ci.openshift.org/openshift/release:golang-1.16 AS builder
WORKDIR /go/src/github.com/openshift-eng/shodan
COPY . .
ENV GO_PACKAGE github.com/openshift-eng/shodan
RUN make build --warn-undefined-variables

FROM registry.ci.openshift.org/ocp/4.6:base
COPY --from=builder /go/src/github.com/openshift-eng/shodan/shodan /usr/bin/

