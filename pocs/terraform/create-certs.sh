#!/bin/bash
kubectl create namespace management 
openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes -keyout registry.key -out registry.crt -subj "/CN=localregistry-docker-registry.management.svc.cluster.local" -addext "subjectAltName=DNS:localregistry-docker-registry.management.svc.cluster.local"
kubectl create secret tls docker-registry-tls-cert -n management --cert=registry.crt --key=registry.key