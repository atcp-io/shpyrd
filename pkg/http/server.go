package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"shpyrd/pkg/kube"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func Hello() string {
	return "Hello, world."
}

func pong(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "pong",
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
		fmt.Println(err.Error)
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

func Run() {
	log.SetLevel(log.DebugLevel)

	kubeConfig, err := kube.AutoDetectConfig()
	if err != nil {
		log.Debug(err)
	}

	// ENV mode only
	runMode := os.Getenv("RUNMODE")
	if runMode == "" {
		kubeConfig.Host = "https://kubernetes.default.svc.cluster.local:54724"
	}
	log.Debug("Kubernetes Host: ", kubeConfig.Host)
	kubeClient, err = kube.KubernetesClient(kubeConfig)
	if err != nil {
		log.Debug(err)
	}

	fmt.Println(gin.Version)
	{
		fmt.Println("Hello World")
	}
	_engine.Run() // listen and serve on 0.0.0.0:8080 (for windows "localhost:8080")
}
