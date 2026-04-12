# Git
GIT_VERSION ?= $(shell git describe --tags --always)
GIT_BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD)
GIT_COMMIT_ID ?= $(shell git rev-parse --short HEAD)

# Image URL to use all building/pushing image targets
REG ?= ghcr.io
REG_NS ?= koordinator-sh
REG_USER ?= ""
REG_PWD ?= ""

KOORDLET_IMG ?= "${REG}/${REG_NS}/koordlet:${GIT_BRANCH}-${GIT_COMMIT_ID}"
KOORD_MANAGER_IMG ?= "${REG}/${REG_NS}/koord-manager:${GIT_BRANCH}-${GIT_COMMIT_ID}"
KOORD_SCHEDULER_IMG ?= "${REG}/${REG_NS}/koord-scheduler:${GIT_BRANCH}-${GIT_COMMIT_ID}"
KOORD_DESCHEDULER_IMG ?= "${REG}/${REG_NS}/koord-descheduler:${GIT_BRANCH}-${GIT_COMMIT_ID}"
KOORD_DEVICE_DAEMON_IMG ?= "${REG}/${REG_NS}/koord-device-daemon:${GIT_BRANCH}-${GIT_COMMIT_ID}"

# --- BuildKit / buildx（与 xcph/portgateway 一致：独立 DOCKER_CONFIG、buildkit-login、buildkitd.toml）
CONTAINER_TOOL ?= docker
BUILDX_LOCAL_BUILDER ?= koordinator-local
BUILDKIT_DOCKER_CONFIG_DIR ?= $(abspath $(CURDIR)/.docker-config.buildkit)
BUILDKIT_DOCKER_CONFIG_ENV := DOCKER_CONFIG="$(BUILDKIT_DOCKER_CONFIG_DIR)"
REGISTRY_ENDPOINT ?= registry.k8s.local:10443
LOCAL_PUSH ?= 0
BUILDX_PUSH_FLAG ?= --push
BUILDX_LOAD_FLAG ?= --load
BUILDX_LOCAL_OUTPUT_FLAG :=
ifeq ($(strip $(LOCAL_PUSH)),1)
  BUILDX_LOCAL_OUTPUT_FLAG := $(BUILDX_PUSH_FLAG)
else
  BUILDX_LOCAL_OUTPUT_FLAG := $(BUILDX_LOAD_FLAG)
endif
BUILDX_EXTRA_HOSTS ?=
BUILDX_HOST_IP ?=
BUILDX_HOST_NAME ?=
BUILDX_ADD_HOST_SINGLE := $(strip $(if $(and $(strip $(BUILDX_HOST_IP)),$(strip $(BUILDX_HOST_NAME))),--add-host=$(strip $(BUILDX_HOST_NAME)):$(strip $(BUILDX_HOST_IP)),))
BUILDX_ADD_HOST_MULTI := $(foreach h,$(BUILDX_EXTRA_HOSTS),$(if $(findstring :,$(h)),--add-host=$(h),))
BUILDX_ADD_HOST_FLAGS := $(strip $(BUILDX_ADD_HOST_SINGLE) $(BUILDX_ADD_HOST_MULTI))
ifeq ($(origin BUILDX_LOCAL_BUILDER_NETWORK_HOST),undefined)
  ifneq ($(strip $(BUILDX_EXTRA_HOSTS))$(strip $(BUILDX_HOST_IP)),)
    BUILDX_LOCAL_BUILDER_NETWORK_HOST := 1
  else
    BUILDX_LOCAL_BUILDER_NETWORK_HOST := 0
  endif
endif
BUILDX_LOCAL_BUILDER_EFFECTIVE := $(BUILDX_LOCAL_BUILDER)$(if $(filter 1,$(strip $(BUILDX_LOCAL_BUILDER_NETWORK_HOST))),-nethost,)
BUILDX_DOCKER_CONTAINER_DRIVER_OPTS :=
ifeq ($(strip $(BUILDX_LOCAL_BUILDER_NETWORK_HOST)),1)
  BUILDX_DOCKER_CONTAINER_DRIVER_OPTS := --driver-opt network=host
endif
BUILDKITD_CONFIG_SRC := $(abspath $(CURDIR)/buildkitd.toml)
BUILDKITD_CONFIG_EFFECTIVE := $(abspath $(CURDIR)/.buildkitd.effective.toml)
DOCKER_BUILD_CONTEXT ?= $(CURDIR)
PLATFORM ?=
DOCKER_BUILD_PLATFORM_FLAGS :=
ifneq ($(strip $(PLATFORM)),)
  DOCKER_BUILD_PLATFORM_FLAGS := --platform=$(PLATFORM)
endif

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.28

AGENT_MODE ?= hostMode
# Set license header files.
LICENSE_HEADER_GO ?= hack/boilerplate/boilerplate.go.txt

PERFGROUPPACKAGE ?= 'github.com/koordinator-sh/koordinator/pkg/koordlet/util/perf_group'
PACKAGES ?= $(shell go list ./... | grep -vE 'vendor|test/e2e|$(PERFGROUPPACKAGE)')

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

LINT_TIMEOUT ?= 15m
DOCKER_BUILDER ?= buildx build

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="$(LICENSE_HEADER_GO)" paths="./apis/..."
	@hack/update-codegen.sh

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet -unsafeptr=false $(PERFGROUPPACKAGE)
	go list ./... | grep -v $(PERFGROUPPACKAGE) | xargs go vet

.PHONY: lint
lint: lint-go lint-license ## Lint all code.

.PHONY: lint-go
lint-go: golangci-lint ## Lint Go code.
	$(GOLANGCI_LINT) run -v --timeout=$(LINT_TIMEOUT)

.PHONY: lint-license
lint-license:
	@hack/update-license-header.sh

.PHONY: test
test: manifests generate fmt vet envtest libpfm ## Run tests.
	@KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" agent_mode=$(AGENT_MODE) go test $(PACKAGES) -race -covermode atomic -coverprofile cover.out
	@KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" agent_mode=$(AGENT_MODE) go test $(PERFGROUPPACKAGE) -covermode atomic -coverprofile tmp.out && cat tmp.out | tail -n +2 >> cover.out && rm tmp.out

.PHONY: fast-test
fast-test: envtest libpfm ## Run tests fast.
	@KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" agent_mode=$(AGENT_MODE) go test $(PACKAGES) -race -covermode atomic -coverprofile cover.out
	@KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" agent_mode=$(AGENT_MODE) go test $(PERFGROUPPACKAGE) -covermode atomic -coverprofile tmp.out && cat tmp.out | tail -n +2 >> cover.out && rm tmp.out

##@ Build

.PHONY: build
build: build-koordlet build-koord-manager build-koord-scheduler build-koord-descheduler build-koord-runtime-proxy build-koord-device-daemon

.PHONY: build-koordlet
build-koordlet: libpfm ## Build koordlet binary.
	go build -o bin/koordlet cmd/koordlet/main.go

.PHONY: build-koord-manager
build-koord-manager: ## Build koord-manager binary.
	go build -o bin/koord-manager cmd/koord-manager/main.go

.PHONY: build-koord-scheduler
build-koord-scheduler: ## Build koord-scheduler binary.
	go build -o bin/koord-scheduler cmd/koord-scheduler/main.go

.PHONY: build-koord-descheduler
build-koord-descheduler: ## Build koord-descheduler binary.
	go build -o bin/koord-descheduler cmd/koord-descheduler/main.go

.PHONY: build-koord-runtime-proxy
build-koord-runtime-proxy: ## Build koord-runtime-proxy binary.
	go build -o bin/koord-runtime-proxy cmd/koord-runtime-proxy/main.go

.PHONY: build-koord-device-daemon
build-koord-device-daemon: ## Build koord-device-daemon binary.
	go build -o bin/koord-device-daemon cmd/koord-device-daemon/main.go

##@ Docker（BuildKit / 与 xcph/portgateway 一致）

.PHONY: docker-buildx-buildkitd-effective
docker-buildx-buildkitd-effective: ## 合并 buildkitd.toml；REGISTRY_ENDPOINT 为纯 IP:port 时追加 insecure 段
	@cp "$(BUILDKITD_CONFIG_SRC)" "$(BUILDKITD_CONFIG_EFFECTIVE)"
	@REG_EP='$(REGISTRY_ENDPOINT)'; \
	if echo "$$REG_EP" | grep -qE '^[0-9]{1,3}(\.[0-9]{1,3}){3}:[0-9]+$$'; then \
	  printf '\n# generated: REGISTRY_ENDPOINT=%s (IP pulls need matching [registry."..."] insecure)\n[registry."%s"]\n  insecure = true\n' "$$REG_EP" "$$REG_EP" >> "$(BUILDKITD_CONFIG_EFFECTIVE)"; \
	fi

.PHONY: docker-buildx-local-ensure
docker-buildx-local-ensure: docker-buildx-buildkitd-effective ## 创建/选用带 buildkitd.toml 的本地 buildx builder
	@mkdir -p "$(BUILDKIT_DOCKER_CONFIG_DIR)"
	@PREV_EP="$(CURDIR)/.buildkitd.endpoint.prev"; \
	PREV_CFG="$(CURDIR)/.buildkitd.config.prev"; \
	CFG_HASH="$$( (sha256sum "$(BUILDKITD_CONFIG_EFFECTIVE)" 2>/dev/null || shasum -a 256 "$(BUILDKITD_CONFIG_EFFECTIVE)") | awk '{print $$1}' )"; \
	NEED_RECREATE=""; \
	BUILDER_EXISTS=""; \
	if $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx inspect $(BUILDX_LOCAL_BUILDER_EFFECTIVE) >/dev/null 2>&1; then \
	  BUILDER_EXISTS="1"; \
	fi; \
	if [ -f "$$PREV_EP" ] && [ "$$(cat "$$PREV_EP" 2>/dev/null)" != "$(REGISTRY_ENDPOINT)" ]; then \
	  NEED_RECREATE="REGISTRY_ENDPOINT changed"; \
	fi; \
	if [ -f "$$PREV_CFG" ] && [ "$$(cat "$$PREV_CFG" 2>/dev/null)" != "$$CFG_HASH" ]; then \
	  NEED_RECREATE="$${NEED_RECREATE:+$$NEED_RECREATE; }buildkitd config changed"; \
	fi; \
	if [ -n "$$BUILDER_EXISTS" ] && { [ ! -f "$$PREV_EP" ] || [ ! -f "$$PREV_CFG" ]; }; then \
	  NEED_RECREATE="$${NEED_RECREATE:+$$NEED_RECREATE; }missing local buildkitd state cache"; \
	fi; \
	if [ -n "$$NEED_RECREATE" ]; then \
	  echo "buildx: $$NEED_RECREATE, recreating builder $(BUILDX_LOCAL_BUILDER_EFFECTIVE)"; \
	  $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx rm $(BUILDX_LOCAL_BUILDER_EFFECTIVE) 2>/dev/null || true; \
	fi; \
	echo '$(REGISTRY_ENDPOINT)' > "$$PREV_EP"; \
	echo "$$CFG_HASH" > "$$PREV_CFG"
	@if [ "$(BUILDX_LOCAL_BUILDER_NETWORK_HOST)" = "1" ]; then \
		echo "buildx: using builder $(BUILDX_LOCAL_BUILDER_EFFECTIVE) with network=host"; \
	fi
	@if ! $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx inspect $(BUILDX_LOCAL_BUILDER_EFFECTIVE) >/dev/null 2>&1; then \
		$(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx create --name $(BUILDX_LOCAL_BUILDER_EFFECTIVE) --driver docker-container \
			--buildkitd-config $(BUILDKITD_CONFIG_EFFECTIVE) $(BUILDX_DOCKER_CONTAINER_DRIVER_OPTS) --bootstrap --use; \
	else \
		$(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) buildx use $(BUILDX_LOCAL_BUILDER_EFFECTIVE); \
	fi

.PHONY: buildkit-login
buildkit-login: ## 登录 REGISTRY_ENDPOINT，凭证写入 BUILDKIT_DOCKER_CONFIG_DIR（不污染 ~/.docker）。用法: make buildkit-login REGISTRY_USER=... REGISTRY_PASS=...
	@test -n "$(strip $(REGISTRY_USER))" || (echo "ERR: REGISTRY_USER is required" >&2; exit 2)
	@test -n "$(strip $(REGISTRY_PASS))" || (echo "ERR: REGISTRY_PASS is required" >&2; exit 2)
	@mkdir -p "$(BUILDKIT_DOCKER_CONFIG_DIR)"
	@REGISTRY_ENDPOINT="$(REGISTRY_ENDPOINT)" REGISTRY_USER="$(REGISTRY_USER)" REGISTRY_PASS="$(REGISTRY_PASS)" OUT_DIR="$(BUILDKIT_DOCKER_CONFIG_DIR)" \
	python3 -c 'import base64, json, os, pathlib, shutil; reg=os.environ["REGISTRY_ENDPOINT"]; user=os.environ["REGISTRY_USER"]; pw=os.environ["REGISTRY_PASS"]; out_dir=pathlib.Path(os.environ["OUT_DIR"]); out_dir.mkdir(parents=True, exist_ok=True); auth_b64=base64.b64encode(f"{user}:{pw}".encode("utf-8")).decode("ascii"); auths={reg: {"auth": auth_b64}, f"https://{reg}": {"auth": auth_b64}, f"http://{reg}": {"auth": auth_b64}}; cfg={"auths": auths}; p=out_dir/"config.json"; p.write_text(json.dumps(cfg, indent=2)+"\n", encoding="utf-8"); print(f"Wrote {p}"); cli_plugins=out_dir/"cli-plugins"; cli_plugins.mkdir(parents=True, exist_ok=True); src=shutil.which("docker-buildx"); dst=cli_plugins/"docker-buildx"; \
 (dst.exists() or dst.is_symlink()) and dst.unlink(); \
 src and dst.symlink_to(src) and print(f"Linked {dst} -> {src}")'

.PHONY: docker-build
docker-build: test docker-build-koordlet docker-build-koord-manager docker-build-koord-scheduler docker-build-koord-descheduler docker-build-koord-device-daemon

.PHONY: docker-build-koordlet
docker-build-koordlet: libpfm docker-buildx-local-ensure ## Build docker image with the koordlet.
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) $(DOCKER_BUILDER) $(BUILDX_LOCAL_OUTPUT_FLAG) $(DOCKER_BUILD_PLATFORM_FLAGS) $(BUILDX_ADD_HOST_FLAGS) $(DOCKER_BUILD_ARGS) --pull -t ${KOORDLET_IMG} -f docker/koordlet.dockerfile .

.PHONY: docker-build-koord-manager
docker-build-koord-manager: docker-buildx-local-ensure ## Build docker image with the koord-manager.
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) $(DOCKER_BUILDER) $(BUILDX_LOCAL_OUTPUT_FLAG) $(DOCKER_BUILD_PLATFORM_FLAGS) $(BUILDX_ADD_HOST_FLAGS) $(DOCKER_BUILD_ARGS) --pull -t ${KOORD_MANAGER_IMG} -f docker/koord-manager.dockerfile .

.PHONY: docker-build-koord-scheduler
docker-build-koord-scheduler: docker-buildx-local-ensure ## Build docker image with the scheduler.
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) $(DOCKER_BUILDER) $(BUILDX_LOCAL_OUTPUT_FLAG) $(DOCKER_BUILD_PLATFORM_FLAGS) $(BUILDX_ADD_HOST_FLAGS) $(DOCKER_BUILD_ARGS) --pull -t ${KOORD_SCHEDULER_IMG} -f docker/koord-scheduler.dockerfile .

.PHONY: docker-build-koord-descheduler
docker-build-koord-descheduler: docker-buildx-local-ensure ## Build docker image with the descheduler.
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) $(DOCKER_BUILDER) $(BUILDX_LOCAL_OUTPUT_FLAG) $(DOCKER_BUILD_PLATFORM_FLAGS) $(BUILDX_ADD_HOST_FLAGS) $(DOCKER_BUILD_ARGS) --pull -t ${KOORD_DESCHEDULER_IMG} -f docker/koord-descheduler.dockerfile .

.PHONY: docker-build-koord-device-daemon
docker-build-koord-device-daemon: docker-buildx-local-ensure ## Build docker image with the koord-device-daemon.
	cd $(DOCKER_BUILD_CONTEXT) && $(BUILDKIT_DOCKER_CONFIG_ENV) $(CONTAINER_TOOL) $(DOCKER_BUILDER) $(BUILDX_LOCAL_OUTPUT_FLAG) $(DOCKER_BUILD_PLATFORM_FLAGS) $(BUILDX_ADD_HOST_FLAGS) $(DOCKER_BUILD_ARGS) --pull -t ${KOORD_DEVICE_DAEMON_IMG} -f docker/koord-device-daemon.dockerfile .
.PHONY: docker-push
docker-push: docker-push-koordlet docker-push-koord-manager docker-push-koord-scheduler docker-push-koord-descheduler docker-push-koord-device-daemon

.PHONY: docker-push-koordlet
docker-push-koordlet: ## Push docker image with the koordlet.
ifneq ($(REG_USER), "")
	docker login -u $(REG_USER) -p $(REG_PWD) ${REG}
endif
	docker push ${KOORDLET_IMG}

.PHONY: docker-push-koord-manager
docker-push-koord-manager: ## Push docker image with the koord-manager.
ifneq ($(REG_USER), "")
	docker login -u $(REG_USER) -p $(REG_PWD) ${REG}
endif
	docker push ${KOORD_MANAGER_IMG}

.PHONY: docker-push-koord-scheduler
docker-push-koord-scheduler: ## Push docker image with the scheduler.
ifneq ($(REG_USER), "")
	docker login -u $(REG_USER) -p $(REG_PWD) ${REG}
endif
	docker push ${KOORD_SCHEDULER_IMG}

.PHONY: docker-push-koord-descheduler
docker-push-koord-descheduler: ## Push docker image with the descheduler.
ifneq ($(REG_USER), "")
	docker login -u $(REG_USER) -p $(REG_PWD) ${REG}
endif
	docker push ${KOORD_DESCHEDULER_IMG}

.PHONY: docker-push-koord-device-daemon
docker-push-koord-device-daemon: ## Push docker image with the koord-device-daemon.
ifneq ($(REG_USER), "")
	docker login -u $(REG_USER) -p $(REG_PWD) ${REG}
endif
	docker push ${KOORD_DEVICE_DAEMON_IMG}
##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image manager=$(KOORD_MANAGER_IMG) scheduler=$(KOORD_SCHEDULER_IMG) descheduler=$(KOORD_DESCHEDULER_IMG) koordlet=$(KOORDLET_IMG)
	@hack/kustomize.sh $(KUSTOMIZE) | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@hack/kustomize.sh $(KUSTOMIZE) | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GINKGO ?= $(LOCALBIN)/ginkgo
HACK_DIR ?= $(PWD)/hack

## Tool Versions
KUSTOMIZE_VERSION ?= v3.8.7
CONTROLLER_TOOLS_VERSION ?= v0.20.0
GOLANGCILINT_VERSION ?= v1.55.2
GINKGO_VERSION ?= v1.16.4

KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	curl -s $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCILINT_VERSION)

.PHONY: ginkgo
ginkgo: $(GINKGO) ## Download ginkgo locally if necessary.
$(GINKGO): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install github.com/onsi/ginkgo/ginkgo@$(GINKGO_VERSION)

.PHONY: libpfm
libpfm:
	@hack/libpfm.sh
