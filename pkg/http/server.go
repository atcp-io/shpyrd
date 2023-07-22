package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"shpyrd/pkg/k8shp"

	"helm.sh/helm/v3/pkg/action"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func Hello() string {
	return "Hello, world."
}

func pong(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "pong",
	})
}

func install_helm(c *gin.Context) {

	c.JSON(http.StatusOK, gin.H{
		"message": "install_helm",
	})
}

func ListNamespaces(client kubernetes.Interface) (*v1.NamespaceList, error) {
	fmt.Println("Get Kubernetes Namespaces")
	namespaces, err := client.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Println(err)
		err = fmt.Errorf("error getting namespaces: %v\n", err)
		return nil, err
	}

	return namespaces, nil
}

func defaultHandler(c *gin.Context) {

	namespaces, err := ListNamespaces(kubeClient)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	/*
		keys := make([]string, len(namespaces.Items))
		i := 0
		for k, ns := range namespaces.Items {
			keys[ns.Name] = k
			i++
		}
	*/

	var bf bytes.Buffer
	for _, namespace := range namespaces.Items {
		fmt.Println(namespace.Name)
		bf.WriteString(namespace.Name)
		for label_key, label := range namespace.Labels {
			fmt.Println("Label: ", label_key, label)
			bf.WriteString(" - Label:")
			bf.WriteString(label_key)
			bf.WriteString(", ")
			bf.WriteString(label)
		}
	}

	c.HTML(http.StatusOK, "views/index", gin.H{
		"items": namespaces.Items,
		"title": "Teste",
	})
	// c.String(http.StatusOK, bf.String())
}

var _engine *gin.Engine

func New() {
	// gin.SetMode(gin.ReleaseMode)
	_engine = gin.Default()
	_engine.SetTrustedProxies(nil)
	_engine.LoadHTMLGlob("ui/templates/**/*")
	_engine.Static("/assets", "./ui/assets")

	_engine.GET("/ping", pong)

	_engine.GET("/", defaultHandler)
}

var kubeClient *kubernetes.Clientset

func getActionConfig(namespace string, config *rest.Config) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)
	var kubeConfig *genericclioptions.ConfigFlags
	// Create the ConfigFlags struct instance with initialized values from ServiceAccount
	kubeConfig = genericclioptions.NewConfigFlags(false)
	kubeConfig.APIServer = &config.Host
	kubeConfig.BearerToken = &config.BearerToken
	kubeConfig.CAFile = &config.CAFile
	kubeConfig.Namespace = &namespace
	if err := actionConfig.Init(kubeConfig, namespace, os.Getenv("HELM_DRIVER"), log.Printf); err != nil {
		return nil, err
	}
	return actionConfig, nil
}

func Run() {
	fmt.Println("Starting...")
	log.SetLevel(log.DebugLevel)

	kubeConfig, err := k8shp.AutoDetectConfig()
	if err != nil {
		log.Debug(err)
	}

	// ENV mode only
	runMode := os.Getenv("RUNMODE")
	if runMode == "" {
		kubeConfig.Host = "https://kubernetes.default.svc.cluster.local:54724"
	}
	log.Debug("Kubernetes Host: ", kubeConfig.Host)
	kubeClient, err = k8shp.KubernetesClient(kubeConfig)
	if err != nil {
		log.Debug(err)
	}

	actionConfig, err := getActionConfig("", kubeConfig)
	listAction := action.NewList(actionConfig)
	releases, err := listAction.Run()
	if err != nil {
		fmt.Println("ERROR - HELM")
		log.Println(err)
	}
	for _, release := range releases {
		log.Println("Release: " + release.Name + " Status: " + release.Info.Status.String())
	}

	fmt.Println(gin.Version)
	{
		fmt.Println("Hello World")
	}
	_engine.Run() // listen and serve on 0.0.0.0:8080 (for windows "localhost:8080")
}
