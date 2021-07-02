all: build
.PHONY: all

# Include the library makefile
include $(addprefix ./vendor/github.com/openshift/build-machinery-go/make/, \
	golang.mk \
	targets/openshift/deps-gomod.mk \
	targets/openshift/images.mk \
)

IMAGE_REGISTRY?=quay.io

$(call build-image,openshift-eng/shodan,$(IMAGE_REGISTRY)/shodan:dev,./Dockerfile,.)

install:
	kubectl apply -f ./manifests
	# You must provide Bugzilla credentials via: kubectl edit configmap/operator-config"
.PHONY: install

uninstall:
	kubectl delete namespace/openshift-eng
.PHONY: uninstall

push:
	docker push quay.io/openshift-eng/shodan:dev
.PHONY: push

clean:
	$(RM) shodan
.PHONY: clean