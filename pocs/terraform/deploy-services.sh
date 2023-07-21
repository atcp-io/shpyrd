#!/bin/bash
echo "Deploying Custom Key"
echo "Deploying Kpack"
kubectl apply -f vendor/kpack-release-0.11.1.yaml
kubectl apply -f kpack/custom.yaml 
kubectl create secret -n management docker-registry image-registry-credentials \
    --docker-server="localregistry-docker-registry.management.svc.cluster.local:30050" \
    --docker-username="user" \
    --docker-password="password"