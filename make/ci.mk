.PHONY: ci-presubmit
## Run all checks (but not Go tests) which should pass before any given pull
## request or change is merged.
##
## @category CI
ci-presubmit: verify-imports verify-errexit verify-boilerplate verify-codegen verify-crds verify-licenses

.PHONY: verify-imports
verify-imports: $(BINDIR)/tools/goimports
	./hack/verify-goimports.sh $<

.PHONY: verify-chart
verify-chart: $(BINDIR)/cert-manager-$(RELEASE_VERSION).tgz
	DOCKER=$(CTR) ./hack/verify-chart-version.sh $<

.PHONY: verify-errexit
verify-errexit:
	./hack/verify-errexit.sh

__PYTHON := python3

.PHONY: verify-boilerplate
verify-boilerplate:
	@command -v $(__PYTHON) >/dev/null || (echo "couldn't find python3 at '$(__PYTHON)', required for $@. Install python3 or set '__PYTHON'" && exit 1)
	$(__PYTHON) hack/verify_boilerplate.py

.PHONY: verify-licenses
verify-licenses: $(BINDIR)/scratch/LATEST-LICENSES
	@diff $(BINDIR)/scratch/LATEST-LICENSES LICENSES >/dev/null || (echo -e "\033[0;33mLICENSES seem to be out of date; update with 'make update-licenses'\033[0m" && exit 1)

.PHONY: verify-crds
verify-crds: | $(DEPENDS_ON_GO) $(BINDIR)/tools/controller-gen $(BINDIR)/tools/yq
	./hack/check-crds.sh $(GO) ./$(BINDIR)/tools/controller-gen ./$(BINDIR)/tools/yq

.PHONY: update-licenses
update-licenses: LICENSES

.PHONY: update-crds
update-crds: generate-test-crds patch-crds | $(BINDIR)/tools/controller-gen

.PHONY: generate-test-crds
generate-test-crds: | $(BINDIR)/tools/controller-gen
	./$(BINDIR)/tools/controller-gen \
		crd \
		paths=./pkg/webhook/handlers/testdata/apis/testgroup/v{1,2}/... \
		output:crd:dir=./pkg/webhook/handlers/testdata/apis/testgroup/crds

PATCH_CRD_OUTPUT_DIR=./deploy/crds
.PHONY: patch-crds
patch-crds: | $(BINDIR)/tools/controller-gen
	./$(BINDIR)/tools/controller-gen \
		schemapatch:manifests=./deploy/crds \
		output:dir=$(PATCH_CRD_OUTPUT_DIR) \
		paths=./pkg/apis/...

.PHONY: verify-codegen
verify-codegen: | k8s-codegen-tools $(DEPENDS_ON_GO)
	VERIFY_ONLY="true" ./hack/k8s-codegen.sh \
		$(GO) \
		./$(BINDIR)/tools/client-gen \
		./$(BINDIR)/tools/deepcopy-gen \
		./$(BINDIR)/tools/informer-gen \
		./$(BINDIR)/tools/lister-gen \
		./$(BINDIR)/tools/defaulter-gen \
		./$(BINDIR)/tools/conversion-gen

.PHONY: update-codegen
update-codegen: | k8s-codegen-tools $(DEPENDS_ON_GO)
	./hack/k8s-codegen.sh \
		$(GO) \
		./$(BINDIR)/tools/client-gen \
		./$(BINDIR)/tools/deepcopy-gen \
		./$(BINDIR)/tools/informer-gen \
		./$(BINDIR)/tools/lister-gen \
		./$(BINDIR)/tools/defaulter-gen \
		./$(BINDIR)/tools/conversion-gen

.PHONY: update-all
## Update CRDs, code generation and licenses to the latest versions.
## This is provided as a convenience to run locally before creating a PR, to ensure
## that everything is up-to-date.
##
## @category Development
update-all: update-crds update-codegen update-licenses
