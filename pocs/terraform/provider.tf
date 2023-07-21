provider "helm" {
  kubernetes {
    config_path = "~/.kube/config"
    config_context = "kind-tkb"
  }
}

provider "kubernetes" {
  config_path    = "~/.kube/config"
  config_context = "kind-tkb"
}

provider "kubectl" {
  load_config_file       = true
  config_context = "kind-tkb"
}