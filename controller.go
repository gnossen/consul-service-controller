package main

import (
	"context"
	"flag"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"log"
	"path/filepath"
	"time"

	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
)

var pollSeconds = flag.Int("poll-seconds", 10, "The number of seconds between state-of-the-world updates.")
var consulEndpoint = flag.String("consul-endpoint", "sidecar:8500", "The endpoint of the consul service to register with.")

var httpClient = &http.Client{}

// TODO: Implement an in-memory datastore and diff against the state of the world to enable deregistration on poll.

func tryRegisterService(hostname string, ip string) error {
	log.Printf("Attempting to register %s at %s.\n", hostname, ip)
	endpoint := fmt.Sprintf("http://%s/v1/agent/service/register", *consulEndpoint)
	body, err := json.Marshal(map[string]string{
		"Name":    hostname,
		"Address": ip,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT", endpoint, bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("Registration was unsuccessful with code %d: %s\n", resp.StatusCode, bodyBytes)
	}
	log.Printf("Successfully registered.")
	return nil
}

func registerService(hostname string, ip string) {
	// TODO: Implement some sort of retry.
	if err := tryRegisterService(hostname, ip); err != nil {
		log.Printf("Error registering %s at %s: %v.\n", hostname, ip, err)
	}
}

func updatePeriodically(clientset *kubernetes.Clientset) {
	for {
		services, err := clientset.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}

		for _, service := range services.Items {
			hostname := service.Name
			namespace := service.Namespace
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				registerService(fmt.Sprintf("%s-%s", hostname, namespace), ingress.IP)
				if namespace == "default" {
					registerService(hostname, ingress.IP)
				}
			}
		}

		time.Sleep(time.Second * time.Duration(*pollSeconds))
	}
}

func main() {
	// TODO; Support in-cluster auth.
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// TODO: Implement delta update in a goroutine.

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	updatePeriodically(clientset)
}
