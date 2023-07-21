#!/bin/bash
echo "Removing Kpack"
kubectl delete -f vendor/kpack-release-0.11.1.yaml
kubectl delete secret -n management image-registry-credentials
kubectl delete -f kpack/custom.yaml 
