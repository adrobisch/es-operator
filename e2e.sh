#!/bin/bash

# Simple script for running the e2e test from your local development machine
# against "some" cluster.
#
# if colima and k3d are found you can create a local cluster setup:
#
# CREATE_LOCAL_CLUSTER=true ./e2e.sh
#
# when running locally you might want to reduce the parallelism:
#
# TEST_PARALLEL=1 ./e2e.sh
#
# you can also provide your own image
# after importing it to the k3d cluster:
#
# make build.docker.local
# k3d images import registry-write.opensource.zalan.do/pandora/es-operator:v0.1.5-37-g446ec75-dirty -c es-operator
# TEST_PARALLEL=1 IMAGE=registry-write.opensource.zalan.do/pandora/es-operator:v0.1.5-37-g446ec75-dirty ./e2e.sh
#
# full local example without deploying the operator in the cluster (per default will be deployed):
#
# TEST_PARALLEL=1 IMAGE=registry-write.opensource.zalan.do/pandora/es-operator:v0.1.5-37-g446ec75-dirty CREATE_LOCAL_CLUSTER=true DEPLOY_OPERATOR=false ./e2e.sh

# fail on errors in this script
set -e

CREATE_LOCAL_CLUSTER="${CREATE_LOCAL_CLUSTER:-false}"

K3D_CLUSTER_NAME="es-operator"
K3D_CREATED=false

if [ "$CREATE_LOCAL_CLUSTER" = true ]; then
    if command -v colima &> /dev/null; then
        echo "colima found..."
        colima start --cpu 6 --memory 12 || true
    fi

    if command -v k3d &> /dev/null; then
        echo "k3d found, creating cluster '$K3D_CLUSTER_NAME'..."
        k3d cluster create "$K3D_CLUSTER_NAME" --wait || true
        kubectl config use-context "k3d-$K3D_CLUSTER_NAME"

        if command -v telepresence &> /dev/null; then
            echo "telepresence found, installing into local cluster"
            telepresence helm install || true
        else
            echo "telepresence not found. you might want to install it: https://telepresence.io/docs/install/client"
        fi

        K3D_CREATED=true
    fi
fi

NAMESPACE="${NAMESPACE:-"es-operator-e2e-$(date +%s)"}"
IMAGE="${IMAGE:-"registry.opensource.zalan.do/pandora/es-operator:latest"}"
SERVICE_ENDPOINT_ES8="${SERVICE_ENDPOINT_ES8:-"http://127.0.0.1:8001/api/v1/namespaces/$NAMESPACE/services/es8-master:9200/proxy"}"
SERVICE_ENDPOINT_ES9="${SERVICE_ENDPOINT_ES9:-"http://127.0.0.1:8001/api/v1/namespaces/$NAMESPACE/services/es9-master:9200/proxy"}"
OPERATOR_ID="${OPERATOR_ID:-"e2e-tests"}"

# start kubectl proxy
kubectl proxy &
PROXY_PID=$!
trap 'kill $PROXY_PID' EXIT
sleep 1 # give proxy time to start

# create namespace and resources
kubectl create ns "$NAMESPACE"
kubectl config set-context --current --namespace=$NAMESPACE
kubectl --namespace "$NAMESPACE" apply -f cmd/e2e/account_cdp.yaml
kubectl --namespace "$NAMESPACE" apply -f deploy/e2e/apply/es8-master.yaml
kubectl --namespace "$NAMESPACE" apply -f deploy/e2e/apply/es8-config.yaml
kubectl --namespace "$NAMESPACE" apply -f deploy/e2e/apply/es8-master-service.yaml
kubectl --namespace "$NAMESPACE" apply -f deploy/e2e/apply/es9-master.yaml
kubectl --namespace "$NAMESPACE" apply -f deploy/e2e/apply/es9-config.yaml
kubectl --namespace "$NAMESPACE" apply -f deploy/e2e/apply/es9-master-service.yaml
# below are cluster level resources, we try but do not fail if things are not working
# rbac
kubectl --namespace "$NAMESPACE" apply -f manifests/rbac.yaml || true
# crds
kubectl --namespace "$NAMESPACE" apply -f docs/zalando.org_elasticsearchdatasets.yaml || true
kubectl --namespace "$NAMESPACE" apply -f docs/zalando.org_elasticsearchmetricsets.yaml || true

sed -e "s#{{{NAMESPACE}}}#$NAMESPACE#"  < manifests/operator_service_account.yaml | kubectl --namespace "$NAMESPACE" apply -f -

DEPLOY_OPERATOR="${DEPLOY_OPERATOR:-true}"

if [ "$DEPLOY_OPERATOR" = true ]; then
    echo "deploying operator..."
    sed -e "s#{{{NAMESPACE}}}#$NAMESPACE#" \
        -e "s#{{{IMAGE}}}#$IMAGE#" \
        -e "s#{{{OPERATOR_ID}}}#$OPERATOR_ID#" < manifests/es-operator.yaml \
        | kubectl --namespace "$NAMESPACE" apply -f -
else
    echo "not deploying operator."
    echo "you can run it with: telepresence quit || true && telepresence connect -n $NAMESPACE && go build && KUBECONFIG=~/.kube/config ./es-operator --debug --operator-id=e2e-tests"
fi

TEST_PARALLEL="${TEST_PARALLEL:-64}"

# run e2e tests
ES_SERVICE_ENDPOINT_ES8=$SERVICE_ENDPOINT_ES8 \
    ES_SERVICE_ENDPOINT_ES9=$SERVICE_ENDPOINT_ES9 \
    E2E_NAMESPACE="$NAMESPACE" \
    OPERATOR_ID="$OPERATOR_ID" \
    KUBECONFIG=~/.kube/config go test -v -parallel $TEST_PARALLEL ./cmd/e2e/...

if [ "$K3D_CREATED" = true ]; then
    echo "Deleting k3d cluster '$K3D_CLUSTER_NAME'..."
    k3d cluster delete "$K3D_CLUSTER_NAME"
else
    kubectl delete ns "$NAMESPACE"
fi
