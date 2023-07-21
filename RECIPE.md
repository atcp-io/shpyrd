### Local Cluster

#### Create a cluster using kind on local machine
```
cd ./contrib/local-cluster
./create-cluster
```

### Bootstrap GitOps

```
flux bootstrap github \
  --owner=iugu-private \
  --repository=devops-infra \
  --branch=main \
  --path=./pocs/kust/clusters/develop \
  --components-extra=image-reflector-controller,image-automation-controller \
  --read-write-key
```

Move to Terraform? Or keep independently
https://registry.terraform.io/providers/fluxcd/flux/latest/docs/guides/github

### Weaver

```
kubectl -n flux-system port-forward svc/weave-gitops 9001:9001
```

### Expose Ingress (Local Development)

```
kubectl -n ingress-nginx port-forward svc/ingress-nginx-controller 8080:80
```

### References

Flux EKS GitOps Config
https://github.com/aws-samples/flux-eks-gitops-config

Karpenter and Kubecost
https://catalog.workshops.aws/eks-blueprints-terraform/en-US/061-autoscaling-karpenter/5-kubecost

AWS EKS Private CA Controller
https://aws.amazon.com/blogs/security/tls-enabled-kubernetes-clusters-with-acm-private-ca-and-amazon-eks-2/

AWS Kubernete Controllers (ACK)
https://aws.amazon.com/blogs/containers/aws-controllers-for-kubernetes-ack/

### Day-2 Operations and HOW'to

How to reset counters for a halted installation:
https://github.com/fluxcd/flux2/discussions/3851

```
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo add grafana https://grafana.github.io/helm-charts
helm repo update
```


```

helm install prometheus prometheus-community/prometheus --namespace monitoring --version 22.3.0

kubectl create namespace monitoring
helm install prometheus prometheus-community/prometheus \
--namespace monitoring \
--set alertmanager.persistentVolume.storageClass="local-path" \
--set server.persistentVolume.storageClass="local-path"

export POD_NAME=$(kubectl get pods --namespace monitoring -l "app.kubernetes.io/name=prometheus,app.kubernetes.io/instance=prometheus" -o jsonpath="{.items[0].metadata.name}")
  kubectl --namespace monitoring port-forward $POD_NAME 9090

```

helm install grafana grafana/grafana \
--namespace monitoring \
--set persistence.storageClassName="standard" \
--set persistence.enabled=true \
--set adminPassword=graf101010 \
--values grafana.yml \
--set service.type=LoadBalancer
```

helm upgrade grafana grafana/grafana \
--namespace monitoring \
--set persistence.storageClassName="standard" \
--set persistence.enabled=true \
--set adminPassword=graf101010 \
--values grafana.yml \
--set service.type=LoadBalancer

helm install prometheus prometheus-community/kube-prometheus-stack --namespace monitoring --set alertmanager.persistentVolume.storageClass="local-path" \
--set server.persistentVolume.storageClass="local-path"


helm install grafana grafana/grafana \--namespace monitoring \
--set persistence.storageClassName="gp2" \
--set persistence.enabled=true \
--set adminPassword=NduJZXdaTCdduaPbNoxzrAkRdnyKCDiL \
--values grafana.yml \
-f options.yml \
--set service.type=LoadBalancer


```
