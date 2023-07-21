resource "kubernetes_namespace" "monitoring" {
  metadata {
    name = "monitoring"

    labels = {
      group = "infrastructure"
    }
  }
}

/*
resource "kubernetes_namespace" "management" {
  metadata {
    name = "management"

    labels = {
      group = "infrastructure"
    }
  }
}
*/

/*
resource "helm_release" "prometheus" {
  name = "prometheus"

  chart      = "prometheus-community/kube-prometheus-stack"
  namespace  = "monitoring"

  values = [
    "${file("config/kube-prometheus-stack.yaml")}"
  ]

  set {
    name = "alertmanager.persistentVolume.storageClass"
    value = "local-path"
  }

  set {
    name = "server.persistentVolume.storageClass"
    value = "local-path"
  }
}

resource "helm_release" "loki" {
  name = "loki"

  # repository  = "https://grafana.github.io/helm-charts"
  chart      = "grafana/loki-stack"
  namespace  = "monitoring"

  values = [
    "${file("config/loki.yaml")}"
  ]
}
*/

resource "helm_release" "localregistry" {
  name = "localregistry"
  repository = "https://helm.twun.io"
  namespace = "management"
  chart = "docker-registry"

  set {
    name = "service.type"
    value = "NodePort,service.nodePort=30050,service.port=30050"
  }

  set {
    name = "persistence.enabled"
    value = "true"
  }

  set {
    name = "persistence.deleteEnabled"
    value = "true"
  }

  set {
    name = "secrets.htpasswd"
    value = "user:$2y$05$Ue5dboOfmqk6Say31Sin9uVbHWTl8J1Sgq9QyAEmFQRnq1TPfP1n2"
  }

/*
  set {
    name = "tlsSecretName"
    value = "docker-registry-tls-cert"
  }
*/
}

resource "kubernetes_secret" "image-registry-credentials" {
  metadata {
    name = "image-registry-credentials"
    namespace = "management"
  }

  data = {
    ".dockerconfigjson" = jsonencode({
      auths = {
        "localregistry-docker-registry.management.svc.cluster.local:30050" = {
          "username" = "user"
          "password" = "password"
          "auth"     = base64encode("user:password")
        }
      }
    })
  }

  type = "kubernetes.io/dockerconfigjson"
}

resource "helm_release" "cert-manager" {
  name = "cert-manager"

  repository  = "https://charts.jetstack.io"
  chart       = "cert-manager"
  namespace   = "management"
  version     = "v1.12.0"

  values = [
    "${file("config/cert-manager.yaml")}"
  ]

  set {
    name = "installCRDs"
    value = "true"
  }
}

resource "helm_release" "trust-manager" {
  name = "trust-manager"

  repository  = "https://charts.jetstack.io"
  chart       = "trust-manager"
  namespace   = "management"
  version     = "v0.5.0"

  values = [
    "${file("config/trust-manager.yaml")}"
  ]

  set {
    name = "installCRDs"
    value = "true"
  }

  set {
    name = "app.trust.namespace"
    value =" management"
  }
}