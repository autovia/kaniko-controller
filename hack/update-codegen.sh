#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
# Copyright 2023 Autovia GmbH.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
CODEGEN_PKG=${CODEGEN_PKG:-$(cd "${SCRIPT_ROOT}"; ls -d -1 ../vendor/k8s.io/code-generator 2>/dev/null || echo ../vendor/k8s.io/code-generator)}

source "${CODEGEN_PKG}/kube_codegen.sh"

kube::codegen::gen_helpers \
    --input-pkg-root autovia.io/kaniko-controller/pkg/apis \
    --output-base "${GOPATH}"/src \
    --boilerplate "./boilerplate.go.txt"

kube::codegen::gen_client \
    --with-watch \
    --input-pkg-root autovia.io/kaniko-controller/pkg/apis \
    --output-pkg-root autovia.io/kaniko-controller/pkg/generated \
    --output-base "${GOPATH}"/src \
    --boilerplate "./boilerplate.go.txt" \
    --clientset-name clientset \
    --versioned-name versioned \
    --listers-name listers \
    --informers-name informers \
